package proctree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

func testRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func startedReceipt() CleanupReceipt {
	return CleanupReceipt{
		AttemptID:      "att-1",
		CommandStarted: true,
		CommandPGID:    4242,
		Reaped:         2,
		GroupEmpty:     true,
		CompletedUnix:  1753500000,
	}
}

func abortReceipt() CleanupReceipt {
	return CleanupReceipt{
		AttemptID:     "att-1",
		GroupEmpty:    true,
		CompletedUnix: 1753500000,
	}
}

func TestReceiptRoundTrip(t *testing.T) {
	root := testRoot(t)
	want := startedReceipt()
	if err := PublishReceipt(root, want); err != nil {
		t.Fatalf("PublishReceipt: %v", err)
	}
	got, err := ReadReceipt(root, want.AttemptID)
	if err != nil {
		t.Fatalf("ReadReceipt: %v", err)
	}
	if got != want {
		t.Fatalf("receipt = %+v, want %+v", got, want)
	}
}

// TestReceiptMissingIsDistinct: "no completion fact" is the case recovery must BLOCK on, so it cannot
// be indistinguishable from an ordinary read failure.
func TestReceiptMissingIsDistinct(t *testing.T) {
	root := testRoot(t)
	if _, err := ReadReceipt(root, "att-1"); !errors.Is(err, ErrReceiptMissing) {
		t.Fatalf("err = %v, want ErrReceiptMissing", err)
	}
}

// TestReceiptIsNoClobber: two publishers for one attempt means the ownership model was violated, and
// neither receipt can then be trusted over the other. Overwriting would hide that.
func TestReceiptIsNoClobber(t *testing.T) {
	root := testRoot(t)
	if err := PublishReceipt(root, startedReceipt()); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := PublishReceipt(root, abortReceipt()); !errors.Is(err, ErrReceiptExists) {
		t.Fatalf("second publish: err = %v, want ErrReceiptExists", err)
	}
	// The original must survive untouched.
	got, err := ReadReceipt(root, "att-1")
	if err != nil {
		t.Fatalf("ReadReceipt: %v", err)
	}
	if !got.CommandStarted {
		t.Fatal("the second publish overwrote the first")
	}
}

// TestReceiptIsAttemptBound: a stale entry from another attempt must not unblock a run it says
// nothing about.
func TestReceiptIsAttemptBound(t *testing.T) {
	root := testRoot(t)
	if err := PublishReceipt(root, startedReceipt()); err != nil {
		t.Fatalf("PublishReceipt: %v", err)
	}
	if _, err := ReadReceipt(root, "att-2"); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("err = %v, want ErrReceiptInvalid", err)
	}
}

func TestReceiptSumType(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    CleanupReceipt
		ok   bool
	}{
		{"started", startedReceipt(), true},
		{"abort", abortReceipt(), true},

		{"empty attempt id", CleanupReceipt{GroupEmpty: true, CompletedUnix: 1}, false},
		{"no group-empty proof", CleanupReceipt{AttemptID: "a", CompletedUnix: 1}, false},
		{"no timestamp", CleanupReceipt{AttemptID: "a", GroupEmpty: true}, false},
		{"negative reaped", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, Reaped: -1}, false},
		{"negative pgid", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, CommandPGID: -1}, false},
		{"no command but a pgid", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, CommandPGID: 5}, false},
		{"no command but reaped", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, Reaped: 1}, false},
		{"started without a pgid", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, CommandStarted: true, Reaped: 1}, false},
		{"started with nothing reaped", CleanupReceipt{AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, CommandStarted: true, CommandPGID: 5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate rejected a valid receipt: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrReceiptInvalid) {
				t.Fatalf("Validate: err = %v, want ErrReceiptInvalid", err)
			}
		})
	}
}

// TestReceiptDecodeRevalidates: recovery reads a file that outlived the process that wrote it, so it
// proves the content rather than trusting the writer.
func TestReceiptDecodeRevalidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    wireReceipt
	}{
		{"no group-empty proof", wireReceipt{SchemaVersion: 1, AttemptID: "a", CompletedUnix: 1}},
		{"no command but a pgid", wireReceipt{SchemaVersion: 1, AttemptID: "a", GroupEmpty: true, CompletedUnix: 1, CommandPGID: 3}},
		{"wrong schema", wireReceipt{SchemaVersion: 2, AttemptID: "a", GroupEmpty: true, CompletedUnix: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := canonjson.CanonicalizeValue(tc.w)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if _, err := decodeReceipt(raw); !errors.Is(err, ErrReceiptInvalid) {
				t.Fatalf("err = %v, want ErrReceiptInvalid", err)
			}
		})
	}
}

func TestReceiptRefusesNonCanonicalBytes(t *testing.T) {
	raw, err := startedReceipt().encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"trailing value", append(append([]byte(nil), raw...), []byte(" {}")...)},
		{"leading whitespace", append([]byte(" "), raw...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeReceipt(tc.in); !errors.Is(err, ErrReceiptInvalid) {
				t.Fatalf("err = %v, want ErrReceiptInvalid", err)
			}
		})
	}
}

// TestReceiptAndTerminalMustAgree. The receipt is what RECOVERY reads and the terminal is what the
// COORDINATOR reads; both describe one attempt. If they could diverge, the gate would decide
// differently depending on which path it took, so a disagreement is a fault rather than something to
// reconcile.
func TestReceiptAndTerminalMustAgree(t *testing.T) {
	r := startedReceipt()
	agreeing := Terminal{CommandStarted: true, Exited: true, CommandPGID: r.CommandPGID, Reaped: r.Reaped}
	if err := r.AgreesWith(agreeing); err != nil {
		t.Fatalf("agreeing pair rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		term Terminal
	}{
		{"started disagrees", Terminal{CommandStarted: false}},
		{"pgid disagrees", Terminal{CommandStarted: true, Exited: true, CommandPGID: r.CommandPGID + 1, Reaped: r.Reaped}},
		{"reap count disagrees", Terminal{CommandStarted: true, Exited: true, CommandPGID: r.CommandPGID, Reaped: r.Reaped + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.AgreesWith(tc.term); !errors.Is(err, ErrReceiptInvalid) {
				t.Fatalf("err = %v, want ErrReceiptInvalid", err)
			}
		})
	}
}

// TestReceiptIsRootConfined: the destination is a handle, not a path, so nothing can steer the write
// out of the attempt directory.
func TestReceiptIsRootConfined(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "attempt")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	root, err := os.OpenRoot(sub)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if err := PublishReceipt(root, startedReceipt()); err != nil {
		t.Fatalf("PublishReceipt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, ReceiptFileName)); err != nil {
		t.Fatalf("receipt not in the attempt directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ReceiptFileName)); err == nil {
		t.Fatal("receipt escaped the attempt directory")
	}
	// An escape attempt through the root is refused by the root itself.
	if _, err := root.OpenFile("../"+ReceiptFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400); err == nil {
		t.Fatal("root permitted a write outside the attempt directory")
	}
}
