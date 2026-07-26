package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
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
	// ErrReceiptInvalid covers malformed, non-canonical, oversized, non-regular, or self-inconsistent
	// receipts.
	ErrReceiptInvalid = errors.New("proctree: cleanup receipt invalid")
	// ErrReceiptDurabilityAmbiguous means the entry exists but its durability was never established.
	// It is NOT a success: the design requires post-commit durability ambiguity to block, because a
	// receipt that may not survive a crash cannot license a retry.
	ErrReceiptDurabilityAmbiguous = errors.New("proctree: cleanup receipt durability unconfirmed")
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
// It delegates to atomicfile.InstallInRoot rather than creating the authoritative name directly. The
// earlier version opened `cleanup.receipt.v1.json` itself with O_EXCL and then wrote, synced and
// closed it — so EVERY failure after the open left the authoritative name behind, often holding
// complete readable bytes. Recovery would then find a receipt that publication had reported as
// failed, which is the exact "post-commit durability ambiguity" the design says must BLOCK. Bytes are
// now synced under a non-authoritative staging name and committed by a rooted no-clobber link, so the
// final name appears only once the content is durable.
//
// Two failure classes are distinguished because they demand different things of the caller:
// ErrReceiptExists means someone else published for this attempt, which is an ownership violation;
// ErrReceiptDurabilityAmbiguous means the entry exists but its durability is unconfirmed, which is
// NOT a success and must not produce a successful terminal.
func PublishReceipt(root *os.Root, r CleanupReceipt) error {
	data, err := r.encode()
	if err != nil {
		return err
	}
	return classifyPublishErr(atomicfile.InstallInRoot(root, ReceiptFileName, data, 0o400))
}

// classifyPublishErr maps a publication failure onto the three outcomes the caller must tell apart.
// It is separated so the branch is testable: a PostCommitSyncError cannot be provoked through the
// filesystem on demand, but misclassifying one would silently turn "durability unknown" into
// "published", which is the difference between blocking and licensing a retry.
func classifyPublishErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s", ErrReceiptExists, ReceiptFileName)
	}
	var pce *atomicfile.PostCommitSyncError
	if errors.As(err, &pce) {
		return fmt.Errorf("%w: %v", ErrReceiptDurabilityAmbiguous, err)
	}
	return fmt.Errorf("proctree: publish receipt: %w", err)
}

// ReadReceipt is what recovery calls.
//
// It reads through atomicfile.ReadInRoot, which proves the entry is a REGULAR file, refuses symlinks,
// and opens non-blocking. Each of those matters here rather than being defensive habit: a FIFO at the
// receipt name would otherwise block the open forever — before any byte ceiling could help, and in
// recovery, which is precisely where an unbounded filesystem read cannot be tolerated — and a
// followed symlink could bless a different in-root file after the real no-clobber publish had
// conflicted.
//
// It then RE-CONFIRMS the directory entry before returning. A prior publish may have committed and
// then failed to sync, so the entry can exist while its durability was never established; recovery is
// the party that must not trust such a receipt, and confirming it here is what converts an ambiguous
// commit into a fact.
func ReadReceipt(root *os.Root, attemptID string) (CleanupReceipt, error) {
	raw, err := atomicfile.ReadInRoot(root, ReceiptFileName, MaxReceiptBytes+1)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return CleanupReceipt{}, fmt.Errorf("%w: %s", ErrReceiptMissing, ReceiptFileName)
		case errors.Is(err, atomicfile.ErrNotRegular):
			// A non-regular entry at the authoritative name is corruption, not an absent receipt:
			// reporting it as missing would let a hostile or damaged directory look merely empty.
			return CleanupReceipt{}, fmt.Errorf("%w: %s is not a regular file: %v", ErrReceiptInvalid, ReceiptFileName, err)
		default:
			return CleanupReceipt{}, fmt.Errorf("proctree: read receipt: %w", err)
		}
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
	if err := atomicfile.ConfirmParentInRoot(root, ReceiptFileName); err != nil {
		return CleanupReceipt{}, fmt.Errorf("%w: %v", ErrReceiptDurabilityAmbiguous, err)
	}
	return r, nil
}
