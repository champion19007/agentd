package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

// ErrNoKnownGood reports that a check has no snapshot known to have extracted
// correctly. Without one there is nothing to verify a repair against.
var ErrNoKnownGood = errors.New("no known-good snapshot")

// ErrUnknownSnapshot reports a reference to a snapshot an index does not hold.
var ErrUnknownSnapshot = errors.New("unknown snapshot")

// Snapshot is the raw source content one run observed, addressed by the hash
// of its own bytes.
//
// Snapshots are what make repair honest. A proposed binding is not accepted
// because a model sounded confident; it is accepted because it was run against
// stored evidence and produced what the intent describes. That evidence has to
// be exactly the bytes that were seen, which is what content addressing
// guarantees: an ID that no longer matches its body is detectable.
type Snapshot struct {
	id          SnapshotID
	checkID     CheckID
	contentType string
	body        []byte
	fingerprint SourceFingerprint
	capturedAt  time.Time
	knownGood   bool
}

// Digest returns the content address of body: "sha256:" followed by the
// lowercase hex digest.
func Digest(body []byte) SnapshotID {
	sum := sha256.Sum256(body)
	return SnapshotID("sha256:" + hex.EncodeToString(sum[:]))
}

// NewSnapshot captures body as a snapshot. The identifier is derived from the
// content, never supplied, so two identical fetches produce one identity.
func NewSnapshot(checkID CheckID, contentType string, body []byte, fp SourceFingerprint, at time.Time) (Snapshot, error) {
	if !nonEmpty(string(checkID)) {
		return Snapshot{}, invalidf("snapshot needs a check id")
	}
	if len(body) == 0 {
		return Snapshot{}, invalidf("snapshot of check %q has an empty body", checkID)
	}
	if !nonEmpty(string(fp)) {
		return Snapshot{}, invalidf("snapshot of check %q needs a source fingerprint", checkID)
	}
	stored := make([]byte, len(body))
	copy(stored, body)
	return Snapshot{
		id:          Digest(stored),
		checkID:     checkID,
		contentType: contentType,
		body:        stored,
		fingerprint: fp,
		capturedAt:  at.UTC(),
	}, nil
}

// ID returns the content address.
func (s Snapshot) ID() SnapshotID { return s.id }

// CheckID returns the check this snapshot was captured for.
func (s Snapshot) CheckID() CheckID { return s.checkID }

// ContentType returns the media type reported by the source.
func (s Snapshot) ContentType() string { return s.contentType }

// Body returns a copy of the captured bytes. It is a copy so that a caller
// cannot mutate a snapshot out of agreement with its own address.
func (s Snapshot) Body() []byte {
	out := make([]byte, len(s.body))
	copy(out, s.body)
	return out
}

// Size returns the length of the captured bytes.
func (s Snapshot) Size() int { return len(s.body) }

// Fingerprint returns the shape of the source as observed.
func (s Snapshot) Fingerprint() SourceFingerprint { return s.fingerprint }

// CapturedAt returns when the snapshot was taken.
func (s Snapshot) CapturedAt() time.Time { return s.capturedAt }

// KnownGood reports whether extraction succeeded against this snapshot.
func (s Snapshot) KnownGood() bool { return s.knownGood }

// Verify reports whether the body still hashes to the identifier. A mismatch
// means the store handed back corrupt evidence, which must never be used to
// verify a repair.
func (s Snapshot) Verify() error {
	if got := Digest(s.body); got != s.id {
		return invalidf("snapshot %q hashes to %q; stored evidence is corrupt", s.id, got)
	}
	return nil
}

// SnapshotIndex is the set of snapshots retained for one check, and the place
// the retention invariant lives.
//
// Invariant: at least one known-good snapshot is retained for as long as the
// check has ever had one. Pruning will exceed its own retention count rather
// than break it.
type SnapshotIndex struct {
	checkID CheckID
	snaps   []Snapshot
}

// NewSnapshotIndex creates an empty index for a check.
func NewSnapshotIndex(checkID CheckID) *SnapshotIndex {
	return &SnapshotIndex{checkID: checkID}
}

// CheckID returns the check this index belongs to.
func (x *SnapshotIndex) CheckID() CheckID { return x.checkID }

// Len returns how many snapshots are retained.
func (x *SnapshotIndex) Len() int { return len(x.snaps) }

// All returns the retained snapshots, newest first. The slice is a copy.
func (x *SnapshotIndex) All() []Snapshot {
	out := make([]Snapshot, len(x.snaps))
	copy(out, x.snaps)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].capturedAt.After(out[j].capturedAt)
	})
	return out
}

// Add retains a snapshot. Adding the same content twice is not an error and
// does not duplicate: content addressing means it is the same snapshot.
func (x *SnapshotIndex) Add(s Snapshot) error {
	if s.checkID != x.checkID {
		return invalidf("snapshot of check %q cannot be added to the index for check %q", s.checkID, x.checkID)
	}
	if err := s.Verify(); err != nil {
		return err
	}
	for _, existing := range x.snaps {
		if existing.id == s.id {
			return nil
		}
	}
	x.snaps = append(x.snaps, s)
	return nil
}

// MarkKnownGood records that extraction succeeded against a snapshot, making
// it eligible to verify a future repair.
func (x *SnapshotIndex) MarkKnownGood(id SnapshotID) error {
	for i := range x.snaps {
		if x.snaps[i].id == id {
			x.snaps[i].knownGood = true
			return nil
		}
	}
	return ErrUnknownSnapshot
}

// LatestKnownGood returns the most recently captured snapshot that extracted
// correctly.
func (x *SnapshotIndex) LatestKnownGood() (Snapshot, error) {
	var best Snapshot
	found := false
	for _, s := range x.snaps {
		if !s.knownGood {
			continue
		}
		if !found || s.capturedAt.After(best.capturedAt) {
			best, found = s, true
		}
	}
	if !found {
		return Snapshot{}, ErrNoKnownGood
	}
	return best, nil
}

// Prune drops the oldest snapshots until at most keep remain, and returns the
// identifiers it dropped so that a caller can delete their bodies.
//
// The most recent known-good snapshot is never dropped, even when keeping it
// means exceeding keep. Losing it would leave Agentd unable to verify any
// future repair for this check, which is a far worse outcome than holding one
// extra body on disk.
func (x *SnapshotIndex) Prune(keep int) []SnapshotID {
	if keep < 0 {
		keep = 0
	}
	if len(x.snaps) <= keep {
		return nil
	}

	protected := SnapshotID("")
	if kg, err := x.LatestKnownGood(); err == nil {
		protected = kg.id
	}

	ordered := x.All() // newest first
	var kept []Snapshot
	var dropped []SnapshotID
	for _, s := range ordered {
		if len(kept) < keep || s.id == protected {
			kept = append(kept, s)
			continue
		}
		dropped = append(dropped, s.id)
	}

	x.snaps = kept
	return dropped
}

// CheckInvariants reports whether the index's invariants hold.
func (x *SnapshotIndex) CheckInvariants(everHadKnownGood bool) error {
	seen := make(map[SnapshotID]bool, len(x.snaps))
	for _, s := range x.snaps {
		if seen[s.id] {
			return invalidf("check %q retains snapshot %q twice", x.checkID, s.id)
		}
		seen[s.id] = true
		if s.checkID != x.checkID {
			return invalidf("check %q retains a snapshot belonging to check %q", x.checkID, s.checkID)
		}
		if err := s.Verify(); err != nil {
			return err
		}
	}
	if everHadKnownGood {
		if _, err := x.LatestKnownGood(); err != nil {
			return invalidf("check %q has had a known-good snapshot but retains none", x.checkID)
		}
	}
	return nil
}
