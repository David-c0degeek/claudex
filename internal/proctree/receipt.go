package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// ReceiptFileName is the durable completion fact recovery requires.
//
// After the coordinator dies there is nothing left to wait on: a reparented supervisor is not a child
// of anything that can reap it, an unheld lease proves only that its owner died, and a persisted PGID
// is refused for PID reuse. So the supervisor publishes this before releasing the lease, and its
// ABSENCE blocks — recovery never guesses that cleanup probably finished.
const ReceiptFileName = "cleanup.receipt.v1.json"

const receiptSchemaVersion = 1

// MaxReceiptBytes bounds what recovery will read. A receipt is a handful of scalars; anything larger
// is not a receipt this writer produced.
const MaxReceiptBytes = 8 << 10

var (
	// ErrReceiptExists is a conflict: something already published for this attempt. It is fail-closed
	// rather than idempotent-overwrite, because two publishers for one attempt means the ownership
	// model was violated and neither receipt can be trusted over the other.
	ErrReceiptExists = errors.New("proctree: cleanup receipt already exists")
	// ErrReceiptMissing means no completion fact exists. Recovery BLOCKS on this.
	ErrReceiptMissing = errors.New("proctree: cleanup receipt missing")
	// ErrReceiptInvalid covers malformed, non-canonical, oversized, or self-inconsistent receipts.
	ErrReceiptInvalid = errors.New("proctree: cleanup receipt invalid")
)

// CleanupReceipt is what the supervisor durably publishes once the owned group is proven empty.
type CleanupReceipt struct {
	// AttemptID binds the receipt to one attempt, so a stale receipt cannot be read as this one's.
	AttemptID string
	// CommandStarted mirrors the TERMINAL fact.
	CommandStarted bool
	// CommandPGID is the owned group, zero when no command started — the group cannot exist before
	// the command does, so a no-command receipt carries no PGID and is distinguishable structurally
	// rather than by trusting the boolean.
	CommandPGID int
	// Reaped counts descendants reaped from the owned group.
	Reaped int
	// GroupEmpty records the ESRCH proof. A receipt is only published when this holds, so a false
	// value is never legitimate; it exists so the proof is stated in the record rather than implied
	// by the record's existence.
	GroupEmpty bool
	// CompletedUnix is when cleanup finished, in seconds.
	CompletedUnix int64
}

type wireReceipt struct {
	SchemaVersion  int    `json:"schema_version"`
	AttemptID      string `json:"attempt_id"`
	CommandStarted bool   `json:"command_started"`
	CommandPGID    int    `json:"command_pgid"`
	Reaped         int    `json:"reaped"`
	GroupEmpty     bool   `json:"group_empty"`
	CompletedUnix  int64  `json:"completed_unix"`
}

// Validate enforces the same sum type TERMINAL uses, for the same reason: this is an authoritative
// fact about an attempt, and one that is merely well-formed is not one that can be true.
func (r CleanupReceipt) Validate() error {
	if r.AttemptID == "" {
		return fmt.Errorf("%w: empty attempt id", ErrReceiptInvalid)
	}
	if r.Reaped < 0 {
		return fmt.Errorf("%w: negative reap count", ErrReceiptInvalid)
	}
	if r.CommandPGID < 0 {
		return fmt.Errorf("%w: negative command PGID", ErrReceiptInvalid)
	}
	if r.CompletedUnix <= 0 {
		return fmt.Errorf("%w: missing completion timestamp", ErrReceiptInvalid)
	}
	// A receipt is published ONLY after the group-empty proof, so its absence here would mean the
	// publisher skipped the step the receipt exists to attest.
	if !r.GroupEmpty {
		return fmt.Errorf("%w: published without the group-empty proof", ErrReceiptInvalid)
	}
	if !r.CommandStarted {
		if r.CommandPGID != 0 {
			return fmt.Errorf("%w: no command but a command PGID", ErrReceiptInvalid)
		}
		if r.Reaped != 0 {
			return fmt.Errorf("%w: no command but reaped descendants", ErrReceiptInvalid)
		}
		return nil
	}
	if r.CommandPGID <= 0 {
		return fmt.Errorf("%w: started command without a command PGID", ErrReceiptInvalid)
	}
	if r.Reaped < 1 {
		return fmt.Errorf("%w: started command with no reaped descendants", ErrReceiptInvalid)
	}
	return nil
}

// AgreesWith checks the receipt and the terminal describe the same attempt outcome.
//
// They are produced by the same supervisor from the same run, so disagreement is not a discrepancy to
// reconcile — it means one of them is wrong and neither can be trusted. Recovery reads the receipt;
// the coordinator reads the terminal; if those two could diverge, the gate would decide differently
// depending on which path it took.
func (r CleanupReceipt) AgreesWith(t Terminal) error {
	if r.CommandStarted != t.CommandStarted {
		return fmt.Errorf("%w: receipt says started=%v, terminal says started=%v", ErrReceiptInvalid, r.CommandStarted, t.CommandStarted)
	}
	if r.CommandPGID != t.CommandPGID {
		return fmt.Errorf("%w: receipt PGID %d, terminal PGID %d", ErrReceiptInvalid, r.CommandPGID, t.CommandPGID)
	}
	if r.Reaped != t.Reaped {
		return fmt.Errorf("%w: receipt reaped %d, terminal reaped %d", ErrReceiptInvalid, r.Reaped, t.Reaped)
	}
	return nil
}

func (r CleanupReceipt) encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := canonjson.CanonicalizeValue(wireReceipt{
		SchemaVersion:  receiptSchemaVersion,
		AttemptID:      r.AttemptID,
		CommandStarted: r.CommandStarted,
		CommandPGID:    r.CommandPGID,
		Reaped:         r.Reaped,
		GroupEmpty:     r.GroupEmpty,
		CompletedUnix:  r.CompletedUnix,
	})
	if err != nil {
		return nil, fmt.Errorf("proctree: encode receipt: %w", err)
	}
	if len(b) > MaxReceiptBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrReceiptInvalid, len(b), MaxReceiptBytes)
	}
	return b, nil
}

func decodeReceipt(raw []byte) (CleanupReceipt, error) {
	if len(raw) > MaxReceiptBytes {
		return CleanupReceipt{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrReceiptInvalid, len(raw), MaxReceiptBytes)
	}
	var w wireReceipt
	if err := decodeExactlyOne(raw, &w); err != nil {
		return CleanupReceipt{}, fmt.Errorf("%w: %v", ErrReceiptInvalid, err)
	}
	if w.SchemaVersion != receiptSchemaVersion {
		return CleanupReceipt{}, fmt.Errorf("%w: schema version %d, want %d", ErrReceiptInvalid, w.SchemaVersion, receiptSchemaVersion)
	}
	r := CleanupReceipt{
		AttemptID:      w.AttemptID,
		CommandStarted: w.CommandStarted,
		CommandPGID:    w.CommandPGID,
		Reaped:         w.Reaped,
		GroupEmpty:     w.GroupEmpty,
		CompletedUnix:  w.CompletedUnix,
	}
	if err := r.Validate(); err != nil {
		return CleanupReceipt{}, err
	}
	canonical, err := r.encode()
	if err != nil {
		return CleanupReceipt{}, err
	}
	if !bytes.Equal(canonical, raw) {
		return CleanupReceipt{}, fmt.Errorf("%w: bytes are not the canonical encoding of their own content", ErrReceiptInvalid)
	}
	return r, nil
}

// PublishReceipt writes the completion fact through a rooted directory handle.
//
// Rooted, not path-resolved: the supervisor holds a handle to exactly one attempt directory, so the
// destination cannot be chosen by a caller and cannot be redirected by anything that renames a
// component afterwards. No-clobber, because a second publisher for one attempt means the ownership
// model was violated. Durability-confirmed before returning, because the whole point is that this
// survives the death of the process that wrote it — a receipt still in a page cache when the machine
// dies proves nothing, and returning success then would make recovery trust a fact that no longer
// exists.
func PublishReceipt(root *os.Root, r CleanupReceipt) error {
	data, err := r.encode()
	if err != nil {
		return err
	}
	f, err := root.OpenFile(ReceiptFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrReceiptExists, ReceiptFileName)
		}
		return fmt.Errorf("proctree: create receipt: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("proctree: write receipt: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("proctree: sync receipt: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("proctree: close receipt: %w", err)
	}
	// The entry has to be durable too, not just the bytes: a synced file in an unsynced directory can
	// vanish entirely on a crash.
	if err := syncRootDir(root); err != nil {
		return fmt.Errorf("proctree: confirm receipt durability: %w", err)
	}
	return nil
}

// ReadReceipt is what recovery calls. A missing receipt is reported distinctly, because "no
// completion fact" is the case that must BLOCK rather than be treated as an ordinary read error.
func ReadReceipt(root *os.Root, attemptID string) (CleanupReceipt, error) {
	f, err := root.Open(ReceiptFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CleanupReceipt{}, fmt.Errorf("%w: %s", ErrReceiptMissing, ReceiptFileName)
		}
		return CleanupReceipt{}, fmt.Errorf("proctree: open receipt: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxReceiptBytes+1))
	if err != nil {
		return CleanupReceipt{}, fmt.Errorf("proctree: read receipt: %w", err)
	}
	r, err := decodeReceipt(raw)
	if err != nil {
		return CleanupReceipt{}, err
	}
	// Attempt-bound: a receipt for a different attempt is not this attempt's completion fact, and
	// accepting it would let a stale directory entry unblock a run it says nothing about.
	if r.AttemptID != attemptID {
		return CleanupReceipt{}, fmt.Errorf("%w: receipt is for attempt %q, want %q", ErrReceiptInvalid, r.AttemptID, attemptID)
	}
	return r, nil
}
