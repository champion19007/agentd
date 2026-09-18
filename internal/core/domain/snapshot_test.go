package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

func snapshot(t *testing.T, body string, offset time.Duration) domain.Snapshot {
	t.Helper()
	s, err := domain.NewSnapshot("chk-1", "text/html", []byte(body), "fp-v1", at(offset))
	if err != nil {
		t.Fatalf("NewSnapshot(%q): %v", body, err)
	}
	return s
}

func TestSnapshotIsContentAddressed(t *testing.T) {
	a := snapshot(t, "<html>price 49</html>", 0)
	same := snapshot(t, "<html>price 49</html>", time.Hour)
	other := snapshot(t, "<html>price 59</html>", 0)

	if a.ID() != same.ID() {
		t.Error("identical bodies captured at different times should share one identity")
	}
	if a.ID() == other.ID() {
		t.Error("different bodies must not share an identity")
	}
	if a.ID() != domain.Digest([]byte("<html>price 49</html>")) {
		t.Errorf("ID = %q, want the digest of its own body", a.ID())
	}
	if err := a.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestSnapshotBodyCannotBeMutatedThroughItsAccessor matters because a snapshot
// whose bytes no longer match its address is corrupt evidence, and corrupt
// evidence would let an unverified repair look verified.
func TestSnapshotBodyCannotBeMutatedThroughItsAccessor(t *testing.T) {
	s := snapshot(t, "<html>price 49</html>", 0)

	body := s.Body()
	body[0] = 'X'

	if err := s.Verify(); err != nil {
		t.Errorf("mutating the returned slice corrupted the snapshot: %v", err)
	}
}

func TestNewSnapshotRejectsIncompleteEvidence(t *testing.T) {
	tests := []struct {
		name        string
		checkID     domain.CheckID
		body        []byte
		fingerprint domain.SourceFingerprint
	}{
		{"no check", "", []byte("x"), "fp-v1"},
		{"empty body", "chk-1", nil, "fp-v1"},
		{"no fingerprint", "chk-1", []byte("x"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := domain.NewSnapshot(tt.checkID, "text/html", tt.body, tt.fingerprint, base); err == nil {
				t.Error("NewSnapshot accepted incomplete evidence")
			}
		})
	}
}

func TestSnapshotIndexDeduplicatesByContent(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")

	if err := x.Add(snapshot(t, "same", 0)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := x.Add(snapshot(t, "same", time.Hour)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if got := x.Len(); got != 1 {
		t.Errorf("Len = %d, want 1; the same content is the same snapshot", got)
	}
	mustInvariants(t, x.CheckInvariants(false))
}

func TestSnapshotIndexRejectsAnotherChecksSnapshot(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-2")

	if err := x.Add(snapshot(t, "body", 0)); err == nil {
		t.Error("Add accepted a snapshot belonging to another check")
	}
}

func TestLatestKnownGoodIsTheMostRecentOne(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")
	older := snapshot(t, "older", 0)
	newer := snapshot(t, "newer", time.Hour)
	for _, s := range []domain.Snapshot{older, newer} {
		if err := x.Add(s); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	if _, err := x.LatestKnownGood(); !errors.Is(err, domain.ErrNoKnownGood) {
		t.Fatalf("LatestKnownGood on a fresh index returned %v, want ErrNoKnownGood", err)
	}

	for _, s := range []domain.Snapshot{older, newer} {
		if err := x.MarkKnownGood(s.ID()); err != nil {
			t.Fatalf("MarkKnownGood: %v", err)
		}
	}

	got, err := x.LatestKnownGood()
	if err != nil {
		t.Fatalf("LatestKnownGood: %v", err)
	}
	if got.ID() != newer.ID() {
		t.Error("LatestKnownGood returned the older snapshot")
	}
}

func TestMarkKnownGoodRejectsAnUnknownSnapshot(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")

	if err := x.MarkKnownGood("sha256:nothing"); !errors.Is(err, domain.ErrUnknownSnapshot) {
		t.Errorf("MarkKnownGood returned %v, want ErrUnknownSnapshot", err)
	}
}

// TestPruneNeverDropsTheLastKnownGood is the retention invariant. Losing the
// last known-good snapshot would leave Agentd with nothing to verify a future
// repair against, which is a worse outcome than keeping one extra body.
func TestPruneNeverDropsTheLastKnownGood(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")

	good := snapshot(t, "the good one", 0)
	if err := x.Add(good); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := x.MarkKnownGood(good.ID()); err != nil {
		t.Fatalf("MarkKnownGood: %v", err)
	}

	// Four newer, broken captures arrive. A naive "keep the newest 2" would
	// evict the only snapshot worth keeping.
	for i := 1; i <= 4; i++ {
		s := snapshot(t, "broken "+string(rune('a'+i)), time.Duration(i)*time.Hour)
		if err := x.Add(s); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	dropped := x.Prune(2)

	for _, id := range dropped {
		if id == good.ID() {
			t.Fatal("Prune dropped the last known-good snapshot")
		}
	}
	if _, err := x.LatestKnownGood(); err != nil {
		t.Fatalf("after pruning, LatestKnownGood: %v", err)
	}
	if got := x.Len(); got != 3 {
		t.Errorf("Len = %d, want 3: two retained plus the protected known-good", got)
	}
	mustInvariants(t, x.CheckInvariants(true))
}

func TestPruneDropsTheOldestFirst(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")
	var ids []domain.SnapshotID
	for i := 0; i < 5; i++ {
		s := snapshot(t, "capture "+string(rune('a'+i)), time.Duration(i)*time.Hour)
		if err := x.Add(s); err != nil {
			t.Fatalf("Add: %v", err)
		}
		ids = append(ids, s.ID())
	}

	dropped := x.Prune(2)

	if len(dropped) != 3 {
		t.Fatalf("Prune dropped %d, want 3", len(dropped))
	}
	want := map[domain.SnapshotID]bool{ids[0]: true, ids[1]: true, ids[2]: true}
	for _, id := range dropped {
		if !want[id] {
			t.Errorf("Prune dropped %q, which was not among the three oldest", id)
		}
	}
	mustInvariants(t, x.CheckInvariants(false))
}

func TestPruneIsANoOpBelowTheLimit(t *testing.T) {
	x := domain.NewSnapshotIndex("chk-1")
	if err := x.Add(snapshot(t, "only", 0)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if dropped := x.Prune(5); dropped != nil {
		t.Errorf("Prune dropped %v, want nothing", dropped)
	}
	if got := x.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
}
