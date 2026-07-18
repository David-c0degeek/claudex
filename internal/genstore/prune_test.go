package genstore

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func buildChain(t *testing.T, s *Store, n int) []Record {
	t.Helper()
	var recs []Record
	var head Head
	for i := 1; i <= n; i++ {
		r := appendConst(t, s, head, fmt.Sprintf("gen-%d", i))
		recs = append(recs, r)
		head = r.Head()
	}
	return recs
}

func withGuard(t *testing.T, s *Store, fn func(g *Guard)) {
	t.Helper()
	g, ok, err := Acquire(s.lockPath)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	fn(g)
}

func pruneKeep(t *testing.T, s *Store, k int) {
	t.Helper()
	withGuard(t, s, func(g *Guard) {
		if err := s.PruneKeep(g, k); err != nil {
			t.Fatalf("PruneKeep(%d): %v", k, err)
		}
	})
}

func writeRaw(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, filePerm); err != nil {
		t.Fatalf("write raw %q: %v", path, err)
	}
}

func certRoot(t *testing.T, s *Store) (uint64, bool) {
	t.Helper()
	cert, has, err := s.selectCert()
	if err != nil {
		t.Fatalf("selectCert: %v", err)
	}
	return cert.Root, has
}

// PruneKeep bounds the chain to K, publishes a floor certificate, deletes generations below
// the root, and Latest/Append continue against the pruned head.
func TestPruneKeepBoundsChainAndRecovers(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 5) // gens 1..5
	pruneKeep(t, s, 2)  // keep 2 → root = gen 4

	for g := uint64(1); g <= 3; g++ {
		if _, err := os.Stat(genFile(s, g)); !os.IsNotExist(err) {
			t.Fatalf("generation %d should be pruned", g)
		}
	}
	root, has := certRoot(t, s)
	if !has || root != 4 {
		t.Fatalf("certificate root = %d (has=%v), want 4", root, has)
	}
	head, ok, err := s.Latest()
	if err != nil || !ok || head.Generation != 5 {
		t.Fatalf("Latest after prune = %+v ok=%v err=%v, want gen 5", head, ok, err)
	}
	// Append continues at head+1 (generation numbers are not reused).
	r6 := appendConst(t, s, head.Head(), "gen-6")
	if r6.Generation != 6 {
		t.Fatalf("append after prune generation = %d, want 6", r6.Generation)
	}
}

// The gen-1 base case (unpruned store) recovers unchanged, and a re-prune keeping the full
// length publishes no certificate.
func TestPruneKeepUnprunedBaseCase(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 3)
	pruneKeep(t, s, 5) // K >= length: no certificate
	if _, has := certRoot(t, s); has {
		t.Fatal("no certificate should be published when K >= chain length")
	}
	head, ok, err := s.Latest()
	if err != nil || !ok || head.Generation != 3 {
		t.Fatalf("Latest = %+v ok=%v err=%v, want gen 3", head, ok, err)
	}
}

func TestPruneKeepRejectsZero(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 3)
	withGuard(t, s, func(g *Guard) {
		if err := s.PruneKeep(g, 0); !errors.Is(err, ErrInvalidKeep) {
			t.Fatalf("PruneKeep(0) err = %v, want ErrInvalidKeep", err)
		}
	})
}

// A torn certificate (a crash mid-publish) reserves its sequence; the retry allocates the
// NEXT sequence and re-certifies the same floor (no collision), and the torn one is cleaned up.
func TestPruneTornCertReservesSequence(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 5)
	writeRaw(t, s.certPath(1), []byte("torn-not-a-valid-certificate")) // a pre-existing torn cert seq 1

	pruneKeep(t, s, 2)

	if _, err := os.Stat(s.certPath(1)); !os.IsNotExist(err) {
		t.Fatal("the torn (now obsolete) certificate 1 should be deleted on reconcile")
	}
	if _, err := os.Stat(s.certPath(2)); err != nil {
		t.Fatalf("the retry should allocate certificate sequence 2: %v", err)
	}
	root, has := certRoot(t, s)
	if !has || root != 4 {
		t.Fatalf("re-certified root = %d (has=%v), want 4", root, has)
	}
	if head, ok, err := s.Latest(); err != nil || !ok || head.Generation != 5 {
		t.Fatalf("Latest = %+v ok=%v err=%v, want gen 5", head, ok, err)
	}
}

// A certificate published but with the deletion NOT (fully) done is finished on re-run,
// WITHOUT publishing a new certificate (retained length is already <= K).
func TestPrunePartialDeletionFinishedOnRerun(t *testing.T) {
	s := newStore(t)
	recs := buildChain(t, s, 5)
	// Publish certificate 1 (root gen 4) directly, leaving stragglers below the root.
	file, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 1, RootGeneration: 4, RootDigest: recs[3].Digest})
	writeRaw(t, s.certPath(1), file)
	_ = os.Remove(genFile(s, 1)) // gen 1 already deleted; gens 2,3 remain (partial deletion)

	pruneKeep(t, s, 2) // reconcile finishes the deletion

	for g := uint64(1); g <= 3; g++ {
		if _, err := os.Stat(genFile(s, g)); !os.IsNotExist(err) {
			t.Fatalf("straggler generation %d should be cleaned up on re-run", g)
		}
	}
	if _, err := os.Stat(s.certPath(2)); !os.IsNotExist(err) {
		t.Fatal("no NEW certificate should be published for a cleanup-only reconcile")
	}
	if head, ok, err := s.Latest(); err != nil || !ok || head.Generation != 5 {
		t.Fatalf("Latest = %+v ok=%v err=%v, want gen 5", head, ok, err)
	}
}

// The certified root advances monotonically across successive prunes.
func TestPruneMonotonicRoot(t *testing.T) {
	s := newStore(t)
	recs := buildChain(t, s, 5)
	pruneKeep(t, s, 2) // root = gen 4
	root1, _ := certRoot(t, s)

	head := recs[4].Head()
	for i := 6; i <= 8; i++ {
		r := appendConst(t, s, head, fmt.Sprintf("gen-%d", i))
		head = r.Head()
	}
	pruneKeep(t, s, 2) // chain gen4..gen8 (5 members) → root = gen 7
	root2, _ := certRoot(t, s)
	if root2 <= root1 {
		t.Fatalf("certified root did not advance: %d -> %d", root1, root2)
	}
}

// A torn certificate at the maximum sequence forces the next allocation past the ceiling.
func TestPruneCertSeqCeiling(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 5)
	writeRaw(t, s.certPath(maxCertSeq), []byte("torn")) // occupies the top slot
	withGuard(t, s, func(g *Guard) {
		err := s.PruneKeep(g, 2)
		if err == nil || !strings.Contains(err.Error(), "ceiling") {
			t.Fatalf("PruneKeep err = %v, want a certificate-sequence-ceiling error", err)
		}
	})
}

// Recovery selects the HIGHEST self-intact supported certificate and applies the taxonomy.
func TestRecoveryCertificateTaxonomy(t *testing.T) {
	t.Run("torn cert skipped, lower valid used", func(t *testing.T) {
		s := newStore(t)
		recs := buildChain(t, s, 5)
		// A valid certificate seq 1 (root gen 4) plus a TORN higher seq 2 → the torn is skipped,
		// the valid lower one is selected.
		file, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 1, RootGeneration: 4, RootDigest: recs[3].Digest})
		writeRaw(t, s.certPath(1), file)
		writeRaw(t, s.certPath(2), []byte("torn"))
		root, has := certRoot(t, s)
		if !has || root != 4 {
			t.Fatalf("selected root = %d (has=%v), want the valid seq-1 root 4", root, has)
		}
	})

	t.Run("non-canonical name fails closed", func(t *testing.T) {
		s := newStore(t)
		buildChain(t, s, 3)
		writeRaw(t, s.certPath(1)[:len(s.certPath(1))-len("000000000001.anchor")]+"abc"+certFileExt, []byte("x"))
		if _, _, err := s.selectCert(); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("non-canonical certificate err = %v, want ErrCorrupt", err)
		}
	})

	t.Run("unsupported version fails closed", func(t *testing.T) {
		s := newStore(t)
		recs := buildChain(t, s, 3)
		file, _ := encodeCert(certBody{AnchorFormatVersion: 999, CertificateSequence: 1, RootGeneration: 2, RootDigest: recs[1].Digest})
		writeRaw(t, s.certPath(1), file)
		if _, _, err := s.selectCert(); !errors.Is(err, ErrUnsupportedAnchor) {
			t.Fatalf("unsupported-version err = %v, want ErrUnsupportedAnchor", err)
		}
	})

	t.Run("missing certified root is ErrCorrupt, no fallback", func(t *testing.T) {
		s := newStore(t)
		recs := buildChain(t, s, 5)
		// A lower valid certificate (root gen 1) AND a higher one whose root (gen 4) is deleted:
		// recovery selects the highest and fails closed, never falling back to the lower.
		lo, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 1, RootGeneration: 1, RootDigest: recs[0].Digest})
		hi, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 2, RootGeneration: 4, RootDigest: recs[3].Digest})
		writeRaw(t, s.certPath(1), lo)
		writeRaw(t, s.certPath(2), hi)
		if err := os.Remove(genFile(s, 4)); err != nil {
			t.Fatalf("remove gen 4: %v", err)
		}
		if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Latest with a missing certified root err = %v, want ErrCorrupt (no fallback)", err)
		}
	})
}

// A lock-free reader that races a concurrent prune (deleting generations below the newly
// certified root) RETRIES via the bracket and succeeds — never ErrCorrupt.
func TestReaderRaceConcurrentPruneRetriesAfterReadDir(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 6)
	done := false
	s.WithReadPause(func(phase string) {
		if phase == "after-readdir" && !done {
			done = true
			pruneKeep(t, s, 2) // concurrent prune under its own guard (the reader is lock-free)
		}
	})
	head, ok, err := s.Latest()
	if err != nil {
		t.Fatalf("Latest raced a prune, err = %v (must retry, never fail)", err)
	}
	if !ok || head.Generation != 6 {
		t.Fatalf("head = %+v, want gen 6", head)
	}
}

// A reader paused while opening a floor record that a concurrent prune then deletes retries
// and succeeds.
func TestReaderRaceConcurrentPruneWhileOpeningFloor(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 6)
	done := false
	s.WithReadPause(func(phase string) {
		if phase == "before-read-2" && !done { // about to open gen 2, which the prune deletes
			done = true
			pruneKeep(t, s, 2) // root = gen 5, deletes gens 1..4
		}
	})
	head, ok, err := s.Latest()
	if err != nil {
		t.Fatalf("Latest raced a prune while opening the floor, err = %v", err)
	}
	if !ok || head.Generation != 6 {
		t.Fatalf("head = %+v, want gen 6", head)
	}
}

// A surviving certificate with NO generations remaining is CORRUPT (the certified root cannot
// be present), never silently an empty store — on the bracketed read, the guarded append path,
// and the PruneKeep guarded path. It flows to ErrCorrupt, not a transient retry loop (the
// scan finds zero generations and the certificate is stable, so nothing "vanished mid-scan").
func TestCertWithNoGenerationsIsCorrupt(t *testing.T) {
	s := newStore(t)
	buildChain(t, s, 4)
	pruneKeep(t, s, 2) // root = gen 3; gens 3,4 remain
	if err := os.Remove(genFile(s, 3)); err != nil {
		t.Fatalf("remove gen 3: %v", err)
	}
	if err := os.Remove(genFile(s, 4)); err != nil {
		t.Fatalf("remove gen 4: %v", err)
	}
	if _, ok, err := s.Latest(); ok || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Latest with a cert but no generations = (ok=%v, err=%v), want ErrCorrupt", ok, err)
	}
	withGuard(t, s, func(g *Guard) {
		if _, err := s.AppendLocked(g, Head{}, func(uint64, string) ([]byte, error) { return []byte("x"), nil }); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("AppendLocked with a cert but no generations err = %v, want ErrCorrupt", err)
		}
		if err := s.PruneKeep(g, 2); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("PruneKeep with a cert but no generations err = %v, want ErrCorrupt", err)
		}
	})
}

// A HIGHEST certificate that is checksum-intact but has an INVALID body is authoritative
// corruption: selection fails closed, NEVER rolling back to a lower valid certificate.
func TestHighestMalformedCertFailsClosedNoFallback(t *testing.T) {
	s := newStore(t)
	recs := buildChain(t, s, 5)
	lo, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 1, RootGeneration: 4, RootDigest: recs[3].Digest})
	writeRaw(t, s.certPath(1), lo)
	// A checksum-intact seq-2 certificate whose body is version 1 but has a zero root
	// generation (invalid) → supportedCorrupt.
	bad, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 2, RootGeneration: 0, RootDigest: strings.Repeat("a", 64)})
	writeRaw(t, s.certPath(2), bad)

	if _, _, err := s.selectCert(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("selectCert with a malformed highest cert err = %v, want ErrCorrupt (no fallback)", err)
	}
	if _, ok, err := s.Latest(); ok || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Latest = (ok=%v, err=%v), want ErrCorrupt", ok, err)
	}
}

// A lock-free reader observing a PARTIAL below-root deletion (a crashed prune left some
// below-root files) roots at the new certificate and succeeds — no corruption, no spurious
// transient — and a subsequent reconcile converges.
func TestReaderAcrossPartialBelowRootDeletion(t *testing.T) {
	s := newStore(t)
	recs := buildChain(t, s, 6)
	file, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: 1, RootGeneration: 5, RootDigest: recs[4].Digest})
	writeRaw(t, s.certPath(1), file)
	_ = os.Remove(genFile(s, 1))
	_ = os.Remove(genFile(s, 3)) // gens 2, 4 still remain below the root

	head, ok, err := s.Latest()
	if err != nil || !ok || head.Generation != 6 {
		t.Fatalf("Latest across a partial below-root deletion = (%+v, ok=%v, err=%v), want gen 6", head, ok, err)
	}
	pruneKeep(t, s, 2) // reconcile converges: finishes the below-root deletion
	for g := uint64(1); g <= 4; g++ {
		if _, serr := os.Stat(genFile(s, g)); !os.IsNotExist(serr) {
			t.Fatalf("below-root generation %d should be cleaned up on reconcile", g)
		}
	}
}

// When the bracket cannot converge (a prune advances the certificate on every attempt), the
// lock-free reader returns the TYPED TRANSIENT ErrConcurrentPrune — NEVER ErrCorrupt.
func TestReaderRaceExhaustsToTransient(t *testing.T) {
	s := newStore(t)
	recs := buildChain(t, s, 5) // gen 4 stays present (nothing is deleted here)
	seq := uint64(1)
	s.WithReadPause(func(phase string) {
		if phase == "after-readdir" {
			seq++ // publish an ever-higher certificate so the identity re-check always fails
			file, _ := encodeCert(certBody{AnchorFormatVersion: anchorFormatVersion, CertificateSequence: seq, RootGeneration: 4, RootDigest: recs[3].Digest})
			writeRaw(t, s.certPath(seq), file)
		}
	})
	_, _, err := s.Latest()
	if !errors.Is(err, ErrConcurrentPrune) {
		t.Fatalf("exhausted read err = %v, want ErrConcurrentPrune", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatal("a concurrent-prune race must never be reported as ErrCorrupt")
	}
}
