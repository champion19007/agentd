package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

func TestDomain_RestoreFunctions(t *testing.T) {
	now := time.Now().UTC()

	// 1. RestoreRun
	runID := domain.RunID("run-restore-1")
	chkID := domain.CheckID("chk-restore-1")
	res := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "100", Type: domain.TypeNumber}}
	fail := domain.Failure{Class: domain.ClassTransient, Code: "timeout", Summary: "timed out"}

	r, err := domain.RestoreRun(domain.RestoredRun{
		ID:                runID,
		CheckID:           chkID,
		Slot:              42,
		DefinitionVersion: 1,
		BindingVersion:    1,
		State:             domain.StateQuiet,
		CreatedAt:         now.Add(-time.Minute),
		StartedAt:         now.Add(-30 * time.Second),
		EndedAt:           now,
		SnapshotID:        "snap-1",
		Result:            res,
		Explanation:       "quiet",
	})
	if err != nil {
		t.Fatalf("RestoreRun failed: %v", err)
	}
	if r.ID() != runID || r.CheckID() != chkID || r.Slot() != 42 || r.TraceID() != string(runID) {
		t.Errorf("RestoreRun field mismatch: %+v", r)
	}
	if r.Explanation() != "quiet" || r.Result().Scalar.Text != "100" || r.Failure() != nil {
		t.Errorf("RestoreRun field mismatch: %+v", r)
	}
	if r.DefinitionVersion() != 1 || r.BindingVersion() != 1 || r.CreatedAt().IsZero() {
		t.Errorf("RestoreRun version/time mismatch: %+v", r)
	}

	// Restore failed run
	rFail, err := domain.RestoreRun(domain.RestoredRun{
		ID:                runID,
		CheckID:           chkID,
		Slot:              43,
		DefinitionVersion: 1,
		BindingVersion:    1,
		State:             domain.StateFailed,
		CreatedAt:         now.Add(-time.Minute),
		StartedAt:         now.Add(-30 * time.Second),
		EndedAt:           now,
		Failure:           &fail,
		Explanation:       "failed",
	})
	if err != nil {
		t.Fatalf("RestoreRun failed run failed: %v", err)
	}
	if rFail.Failure() == nil || rFail.Failure().Code != "timeout" {
		t.Errorf("RestoreRun failure mismatch: %+v", rFail)
	}

	// RestoreRun errors
	if _, err := domain.RestoreRun(domain.RestoredRun{ID: "", CheckID: chkID, State: domain.StateQuiet}); err == nil {
		t.Error("expected error for empty run ID")
	}
	if _, err := domain.RestoreRun(domain.RestoredRun{ID: runID, CheckID: "", State: domain.StateQuiet}); err == nil {
		t.Error("expected error for empty check ID")
	}
	if _, err := domain.RestoreRun(domain.RestoredRun{ID: runID, CheckID: chkID, State: domain.RunState("invalid")}); err == nil {
		t.Error("expected error for invalid run state")
	}

	// 2. RestoreCheck
	def := domain.Definition{
		Version:     1,
		Intent:      domain.ScalarIntent{Label: "price", Purpose: "item price", Type: domain.TypeNumber},
		Source:      domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.com"},
		Schedule:    domain.Schedule{Interval: time.Minute},
		Destination: domain.Destination{Kind: domain.DestinationNone},
		CreatedAt:   now,
	}
	c, err := domain.RestoreCheck(domain.RestoredCheck{
		ID:          chkID,
		Definitions: []domain.Definition{def},
		Active:      1,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("RestoreCheck failed: %v", err)
	}
	if c.ID() != chkID || !c.Enabled() || c.ActiveVersion() != 1 {
		t.Errorf("RestoreCheck mismatch: %+v", c)
	}
	c.Disable()
	if c.Enabled() {
		t.Error("expected check disabled")
	}
	c.Enable()
	if !c.Enabled() {
		t.Error("expected check enabled")
	}

	// RestoreCheck error branches
	if _, err := domain.RestoreCheck(domain.RestoredCheck{ID: "", Definitions: []domain.Definition{def}, Active: 1}); err == nil {
		t.Error("expected error for empty ID")
	}
	if _, err := domain.RestoreCheck(domain.RestoredCheck{ID: chkID, Definitions: nil, Active: 1}); err == nil {
		t.Error("expected error for empty definitions")
	}
	if _, err := domain.RestoreCheck(domain.RestoredCheck{ID: chkID, Definitions: []domain.Definition{def}, Active: 2}); err == nil {
		t.Error("expected error for non-existent active version")
	}

	// 3. RestoreSnapshot
	body := []byte("<html>hello world</html>")
	h := sha256.Sum256(body)
	snapID := domain.SnapshotID("sha256:" + hex.EncodeToString(h[:]))

	snap, err := domain.RestoreSnapshot(domain.RestoredSnapshot{
		ID:          snapID,
		CheckID:     chkID,
		ContentType: "text/html",
		Body:        body,
		Fingerprint: "fp-1",
		CapturedAt:  now,
		KnownGood:   true,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}
	if snap.ID() != snapID || snap.CheckID() != chkID || snap.ContentType() != "text/html" ||
		snap.Size() != len(body) || snap.Fingerprint() != "fp-1" || !snap.KnownGood() || snap.CapturedAt().IsZero() {
		t.Errorf("RestoreSnapshot mismatch: %+v", snap)
	}

	// RestoreSnapshot errors
	if _, err := domain.RestoreSnapshot(domain.RestoredSnapshot{ID: "corrupted-id", CheckID: chkID, ContentType: "text/html", Body: body, Fingerprint: "fp-1", CapturedAt: now}); err == nil {
		t.Error("expected error for corrupt digest")
	}
	if _, err := domain.RestoreSnapshot(domain.RestoredSnapshot{ID: snapID, CheckID: "", ContentType: "text/html", Body: body, Fingerprint: "fp-1", CapturedAt: now}); err == nil {
		t.Error("expected error for empty check ID")
	}

	// 4. RestoreSnapshotIndex
	snapIdx, err := domain.RestoreSnapshotIndex(chkID, []domain.Snapshot{snap})
	if err != nil {
		t.Fatalf("RestoreSnapshotIndex failed: %v", err)
	}
	if snapIdx.CheckID() != chkID || snapIdx.Len() != 1 {
		t.Errorf("RestoreSnapshotIndex mismatch: %+v", snapIdx)
	}
	if _, err := domain.RestoreSnapshotIndex("", []domain.Snapshot{snap}); err == nil {
		t.Error("expected error for empty check ID in index")
	}

	// Mismatched check ID in index
	otherSnap, _ := domain.NewSnapshot("other-check", "text/html", body, "fp-1", now)
	if _, err := domain.RestoreSnapshotIndex(chkID, []domain.Snapshot{otherSnap}); err == nil {
		t.Error("expected error for mismatched check snapshot")
	}

	// 5. RestoreIncident and RestoreIncidentLog
	incID := domain.IncidentID("inc-restore-1")
	inc, err := domain.RestoreIncident(domain.RestoredIncident{
		ID:          incID,
		CheckID:     chkID,
		State:       domain.IncidentOpen,
		Cause:       fail,
		OpenedAt:    now,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("RestoreIncident failed: %v", err)
	}
	if inc.ID() != incID || inc.CheckID() != chkID {
		t.Errorf("RestoreIncident mismatch: %+v", inc)
	}

	incLog, err := domain.RestoreIncidentLog(chkID, []*domain.Incident{inc})
	if err != nil {
		t.Fatalf("RestoreIncidentLog failed: %v", err)
	}
	if incLog.CheckID() != chkID || len(incLog.All()) != 1 {
		t.Errorf("RestoreIncidentLog mismatch: %+v", incLog)
	}
	if _, err := domain.RestoreIncidentLog("", []*domain.Incident{inc}); err == nil {
		t.Error("expected error for empty check ID in incident log")
	}
}

func TestDomain_RetentionPolicy(t *testing.T) {
	now := time.Now().UTC()
	ret := domain.DefaultRetention()

	if err := ret.Validate(); err != nil {
		t.Fatalf("DefaultRetention invalid: %v", err)
	}
	sweep := domain.Sweep{}
	if !sweep.Empty() {
		t.Error("expected zero sweep to be empty")
	}

	// Custom retention with shorter TTLs and SnapshotsPerCheck = 1
	customRet := domain.Retention{
		RunTTL:            30 * 24 * time.Hour,
		IncidentTTL:       90 * 24 * time.Hour,
		SnapshotsPerCheck: 1,
	}

	// Cutoff calculations
	runCut := customRet.RunCutoff(now)
	if runCut.After(now) {
		t.Errorf("RunCutoff after now: %v", runCut)
	}
	incCut := customRet.IncidentCutoff(now)
	if incCut.After(now) {
		t.Errorf("IncidentCutoff after now: %v", incCut)
	}

	// Run expiration logic
	res := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "100", Type: domain.TypeNumber}}
	rOld, _ := domain.NewRun("run-old", "chk-1", 1, 1, now.Add(-31*24*time.Hour))
	_ = rOld.Start(now.Add(-31*24*time.Hour), 1)
	_ = rOld.Quiet(now.Add(-31*24*time.Hour), "snap-old", res)
	if !customRet.RunIsExpired(rOld, now) {
		t.Error("expected run older than 30d to be expired")
	}

	rFresh, _ := domain.NewRun("run-fresh", "chk-1", 2, 1, now.Add(-time.Hour))
	if customRet.RunIsExpired(rFresh, now) {
		t.Error("expected recent run not to be expired")
	}

	// Non-terminal run never expires
	rRunning, _ := domain.NewRun("run-run", "chk-1", 3, 1, now.Add(-35*24*time.Hour))
	_ = rRunning.Start(now.Add(-35*24*time.Hour), 1)
	if customRet.RunIsExpired(rRunning, now) {
		t.Error("non-terminal run should never expire")
	}

	// Incident expiration logic
	f := domain.Failure{Class: domain.ClassStructural, Code: "broken", Summary: "broken"}
	incOld, _ := domain.NewIncident("inc-old", "chk-1", f, 3, now.Add(-100*24*time.Hour))
	_ = incOld.Abandon("abandoned", now.Add(-95*24*time.Hour))
	if !customRet.IncidentIsExpired(incOld, now) {
		t.Error("expected closed incident older than 90d to be expired")
	}

	// Open incident never expires
	incOpen, _ := domain.NewIncident("inc-open", "chk-1", f, 3, now.Add(-100*24*time.Hour))
	if customRet.IncidentIsExpired(incOpen, now) {
		t.Error("open incident should never expire regardless of age")
	}

	// Expired snapshots logic: known-good is NEVER expired
	body := []byte("<html>sample</html>")
	sOld, _ := domain.NewSnapshot("chk-1", "text/html", body, "fp-1", now.Add(-31*24*time.Hour))
	sOldGood, _ := domain.NewSnapshot("chk-1", "text/html", []byte("good"), "fp-2", now.Add(-30*24*time.Hour))
	sNew, _ := domain.NewSnapshot("chk-1", "text/html", []byte("new"), "fp-3", now.Add(-time.Hour))

	idx := domain.NewSnapshotIndex("chk-1")
	_ = idx.Add(sOld)
	_ = idx.Add(sOldGood)
	_ = idx.Add(sNew)
	_ = idx.MarkKnownGood(sOldGood.ID())

	expired := customRet.ExpiredSnapshots(idx)
	if len(expired) != 1 || expired[0] != sOld.ID() {
		t.Errorf("expected only sOld to expire, got %v", expired)
	}

	// Invalid retentions
	badRet1 := domain.Retention{RunTTL: -1}
	if err := badRet1.Validate(); err == nil {
		t.Error("expected error for negative RunTTL")
	}
	badRet2 := domain.Retention{RunTTL: time.Hour, IncidentTTL: -1}
	if err := badRet2.Validate(); err == nil {
		t.Error("expected error for negative IncidentTTL")
	}
	badRet3 := domain.Retention{RunTTL: time.Hour, IncidentTTL: time.Hour, SnapshotsPerCheck: 0}
	if err := badRet3.Validate(); err == nil {
		t.Error("expected error for zero SnapshotsPerCheck")
	}
}

func TestDomain_ResultAndSecretHelpers(t *testing.T) {
	// Secret helper methods
	sec := domain.NewSecret("my-api-key-12345")
	if sec.Reveal() != "my-api-key-12345" {
		t.Errorf("reveal mismatch: %s", sec.Reveal())
	}
	if sec.String() != "[redacted]" {
		t.Errorf("string should be redacted, got %s", sec.String())
	}
	if sec.GoString() != "[redacted]" {
		t.Errorf("goString should be redacted, got %s", sec.GoString())
	}

	bundle := make(domain.SecretBundle)
	bundle["key1"] = sec
	got, ok := bundle.Get("key1")
	if !ok || got.Reveal() != "my-api-key-12345" {
		t.Errorf("bundle get mismatch: ok=%v, sec=%v", ok, got)
	}
	_, notFound := bundle.Get("nonexistent")
	if notFound {
		t.Error("expected nonexistent secret not found")
	}

	// Extraction Equal
	e1 := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "apple", Type: domain.TypeString}}
	e2 := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "apple", Type: domain.TypeString}}
	e3 := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "banana", Type: domain.TypeString}}
	e4 := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{"name": {Text: "apple", Type: domain.TypeString}}}

	if !e1.Equal(e2) {
		t.Error("e1 should equal e2")
	}
	if e1.Equal(e3) {
		t.Error("e1 should not equal e3")
	}
	if e1.Equal(e4) {
		t.Error("scalar should not equal record")
	}

	// Collection Equal
	c1 := domain.Extraction{
		Kind: domain.IntentCollection,
		Collection: []domain.Record{
			{"val": {Text: "1", Type: domain.TypeNumber}},
			{"val": {Text: "2", Type: domain.TypeNumber}},
		},
	}
	c2 := domain.Extraction{
		Kind: domain.IntentCollection,
		Collection: []domain.Record{
			{"val": {Text: "1", Type: domain.TypeNumber}},
			{"val": {Text: "2", Type: domain.TypeNumber}},
		},
	}
	c3 := domain.Extraction{
		Kind: domain.IntentCollection,
		Collection: []domain.Record{
			{"val": {Text: "1", Type: domain.TypeNumber}},
		},
	}
	if !c1.Equal(c2) {
		t.Error("c1 should equal c2")
	}
	if c1.Equal(c3) {
		t.Error("c1 should not equal c3 (different length)")
	}

	// Extraction Validate
	if err := e1.Validate(); err != nil {
		t.Errorf("e1 validate failed: %v", err)
	}
	invalidExt := domain.Extraction{Kind: domain.IntentKind("unknown")}
	if err := invalidExt.Validate(); err == nil {
		t.Error("expected error for unknown extraction kind")
	}

	// Notification Validate & ContentHash
	now := time.Now().UTC()
	n := domain.Notification{
		CheckID:    "chk-1",
		Severity:   domain.SeverityInfo,
		Subject:    "Check passed",
		Body:       "All fine",
		OccurredAt: now,
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("notification validate failed: %v", err)
	}
	hash1 := n.ContentHash()
	hash2 := n.ContentHash()
	if hash1 == "" || hash1 != hash2 {
		t.Errorf("content hash must be non-empty and deterministic: %s vs %s", hash1, hash2)
	}

	// Invalid notifications
	if err := (domain.Notification{CheckID: ""}).Validate(); err == nil {
		t.Error("expected error for empty check ID in notification")
	}
	if err := (domain.Notification{CheckID: "chk-1", Subject: ""}).Validate(); err == nil {
		t.Error("expected error for empty subject in notification")
	}
	if err := (domain.Notification{CheckID: "chk-1", Subject: "Sub", NeedsDecision: true, IncidentID: ""}).Validate(); err == nil {
		t.Error("expected error for decision notification with empty incident ID")
	}
}

func TestDomain_AuditEvents(t *testing.T) {
	now := time.Now().UTC()
	e := domain.AuditBy("aud-1", now, "operator", domain.ActionCheckCreated, domain.SubjectCheck, "chk-1", "added check")
	if err := e.Validate(); err != nil {
		t.Fatalf("AuditBy validate failed: %v", err)
	}
	if e.Actor != "operator" || e.Detail != "added check" {
		t.Errorf("unexpected audit fields: %+v", e)
	}

	// Invalid audit events
	if err := (domain.AuditEvent{ID: ""}).Validate(); err == nil {
		t.Error("expected error for empty ID in audit")
	}
	if err := (domain.AuditEvent{ID: "1", At: time.Time{}}).Validate(); err == nil {
		t.Error("expected error for zero At in audit")
	}
	if err := (domain.AuditEvent{ID: "1", At: now, Actor: ""}).Validate(); err == nil {
		t.Error("expected error for empty Actor in audit")
	}
	if err := (domain.AuditEvent{ID: "1", At: now, Actor: "act", Action: ""}).Validate(); err == nil {
		t.Error("expected error for empty Action in audit")
	}
	if err := (domain.AuditEvent{ID: "1", At: now, Actor: "act", Action: "add", SubjectKind: ""}).Validate(); err == nil {
		t.Error("expected error for empty SubjectKind in audit")
	}
	if err := (domain.AuditEvent{ID: "1", At: now, Actor: "act", Action: "add", SubjectKind: "chk", SubjectID: ""}).Validate(); err == nil {
		t.Error("expected error for empty SubjectID in audit")
	}
}

func TestDomain_RunStateMachineTransitions(t *testing.T) {
	now := time.Now().UTC()
	r, err := domain.NewRun("run-state-1", "chk-1", 100, 1, now)
	if err != nil {
		t.Fatalf("NewRun failed: %v", err)
	}

	// Invariant check on pending run
	if err := r.CheckInvariants(); err != nil {
		t.Errorf("pending invariants failed: %v", err)
	}

	ext := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "approx", Type: domain.TypeString}}

	// Illegal transition from Pending to Quiet directly (must start first)
	if err := r.Quiet(now, "snap-1", ext); err == nil {
		t.Error("expected error transitioning from Pending directly to Quiet")
	}

	// Start run
	if err := r.Start(now, 1); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Double start should fail
	if err := r.Start(now, 1); err == nil {
		t.Error("expected error starting an already running run")
	}

	// Test Degrade transition
	f := domain.Failure{Class: domain.ClassStructural, Code: "missing_field", Summary: "optional missing"}
	if err := r.Degrade(now, "snap-1", ext, f); err != nil {
		t.Fatalf("Degrade failed: %v", err)
	}

	// Terminal run cannot transition again
	if err := r.Quiet(now, "snap-1", ext); err == nil {
		t.Error("expected error transitioning terminal run to quiet")
	}
	if err := r.Fail(now, f); err == nil {
		t.Error("expected error transitioning terminal run to fail")
	}
	if err := r.Interrupt(now, "interrupted"); err == nil {
		t.Error("expected error transitioning terminal run to interrupt")
	}
	if err := r.SkipOverloaded(now, "overloaded"); err == nil {
		t.Error("expected error transitioning terminal run to skip")
	}

	// Test SkipOverloaded from Pending
	rOverload, _ := domain.NewRun("run-state-2", "chk-1", 101, 1, now)
	if err := rOverload.SkipOverloaded(now, "system overloaded"); err != nil {
		t.Fatalf("SkipOverloaded failed: %v", err)
	}
	if rOverload.State() != domain.StateSkippedOverload {
		t.Errorf("expected StateSkippedOverload, got %s", rOverload.State())
	}

	// Test Interrupt from Running
	rInter, _ := domain.NewRun("run-state-3", "chk-1", 102, 1, now)
	_ = rInter.Start(now, 1)
	if err := rInter.Interrupt(now, "daemon shutdown"); err != nil {
		t.Fatalf("Interrupt failed: %v", err)
	}
	if rInter.State() != domain.StateInterrupted {
		t.Errorf("expected StateInterrupted, got %s", rInter.State())
	}
}

func TestDomain_AuditAndValidationErrors(t *testing.T) {
	now := time.Now().UTC()

	// 1. Audit
	a := domain.AuditBy("ev-1", now, "operator-alice", domain.ActionRepairApproved, domain.SubjectIncident, "inc-1", "Approved manual repair")
	if a.Actor != "operator-alice" || a.At.IsZero() {
		t.Errorf("AuditBy incorrect: %+v", a)
	}
	if !domain.ActionRepairApproved.RequiresHuman() {
		t.Error("ActionRepairApproved should require human")
	}
	if domain.ActionCheckCreated.RequiresHuman() {
		t.Error("ActionCheckCreated should not require human")
	}

	aAgent := domain.Audit("ev-2", now, domain.ActionCheckCreated, domain.SubjectCheck, "chk-1", "Check initialized")
	if aAgent.Actor != domain.ActorAgentd {
		t.Errorf("expected ActorAgentd, got %s", aAgent.Actor)
	}

	// AuditEvent.Validate
	badAudit := domain.AuditEvent{}
	if err := badAudit.Validate(); err == nil {
		t.Error("expected error for empty audit event")
	}
	badAudit2 := domain.AuditEvent{
		ID:          "ev-bad",
		Action:      domain.ActionRepairApproved,
		SubjectKind: domain.SubjectCheck,
		SubjectID:   "chk-1",
		Actor:       domain.ActorAgentd,
		At:          now,
	}
	if err := badAudit2.Validate(); err == nil {
		t.Error("expected error for non-human approving repair in audit")
	}
	goodAudit := domain.AuditEvent{
		ID:          "ev-good",
		Action:      domain.ActionRepairApproved,
		SubjectKind: domain.SubjectIncident,
		SubjectID:   "inc-1",
		Actor:       "operator-alice",
		At:          now,
	}
	if err := goodAudit.Validate(); err != nil {
		t.Errorf("unexpected error for valid audit: %v", err)
	}

	// 2. Failure error formatting
	f1 := domain.Failure{Class: domain.ClassTransient, Summary: "sum1"}
	if f1.Error() == "" {
		t.Error("expected non-empty Error string")
	}
	f2 := domain.Failure{Class: domain.ClassStructural, Code: "e2", Summary: "sum2", Detail: "detailed error"}
	if f2.Error() == "" {
		t.Error("expected non-empty Error string")
	}

	// 3. Check policy methods
	policy := domain.Policy{
		MaxRepairAttempts: 4,
		MaxRetries:        3,
		RetryBackoff:      time.Second,
	}
	if policy.Attempts() != 4 {
		t.Errorf("expected 4 attempts, got %d", policy.Attempts())
	}
	if policy.Retries() != 3 {
		t.Errorf("expected 3 retries, got %d", policy.Retries())
	}
	b0 := policy.Backoff(0)
	if b0 != time.Second {
		t.Errorf("attempt 0 backoff should fall back to 1s, got %v", b0)
	}
	b1 := policy.Backoff(1)
	if b1 != time.Second {
		t.Errorf("attempt 1 backoff should be 1s, got %v", b1)
	}
	b2 := policy.Backoff(2)
	if b2 != 2*time.Second {
		t.Errorf("attempt 2 backoff should be 2s, got %v", b2)
	}
	b10 := policy.Backoff(10)
	if b10 > time.Hour {
		t.Errorf("attempt 10 backoff should cap at Max 1h, got %v", b10)
	}

	// 4. Source Spec SecretRefs
	src := domain.SourceSpec{
		Kind:          domain.SourceHTTP,
		URL:           "https://example.com/api",
		SecretHeaders: map[string]domain.SecretRef{"Authorization": "api-key"},
	}
	refs := src.SecretRefs()
	if len(refs) != 1 || refs[0] != "api-key" {
		t.Errorf("expected secret ref 'api-key', got %v", refs)
	}

	// 5. Check staleness & invariants
	sched := domain.Schedule{Interval: time.Minute, CatchUp: domain.CatchUpSkip}
	def := domain.Definition{
		Version:   1,
		Source:    src,
		Schedule:  sched,
		Intent:    domain.ScalarIntent{Label: "price", Purpose: "product price", Type: domain.TypeNumber},
		CreatedAt: now,
	}
	chk, err := domain.NewCheck("chk-cov-1", def)
	if err != nil {
		t.Fatalf("NewCheck failed: %v", err)
	}

	// Staleness
	st1 := domain.CheckStaleness(chk, nil, now, 1.5)
	if st1.IsStale {
		t.Error("freshly created check should not be stale immediately")
	}

	// Check with old run
	rRecent, _ := domain.NewRun("run-s-1", chk.ID(), 1, 1, now.Add(-10*time.Minute))
	_ = rRecent.Start(now.Add(-10*time.Minute), 1)
	_ = rRecent.Quiet(now.Add(-10*time.Minute), "snap-1", domain.Extraction{Kind: domain.IntentScalar})
	st2 := domain.CheckStaleness(chk, rRecent, now, 1.5)
	if !st2.IsStale {
		t.Errorf("expected stale check after 10m on 1m schedule: %+v", st2)
	}

	// CheckInvariants
	if err := chk.CheckInvariants(); err != nil {
		t.Errorf("CheckInvariants failed on valid check: %v", err)
	}
}

func TestDomain_IncidentAndRepairCoverage(t *testing.T) {
	now := time.Now().UTC()
	chkID := domain.CheckID("chk-inc-cov")
	f := domain.Failure{Class: domain.ClassStructural, Code: "selector_not_found", Summary: "Price element gone"}

	inc, err := domain.NewIncident("inc-cov-1", chkID, f, 3, now)
	if err != nil {
		t.Fatalf("NewIncident failed: %v", err)
	}
	if inc.Cause().Code != "selector_not_found" || !inc.OpenedAt().Equal(now) {
		t.Errorf("Incident fields mismatch: cause=%+v, openedAt=%v", inc.Cause(), inc.OpenedAt())
	}

	// Record failed attempt
	att, err := inc.RecordAttempt(domain.AttemptUnverified, "selector produced empty text", now.Add(time.Second))
	if err != nil {
		t.Fatalf("RecordAttempt failed: %v", err)
	}
	if att.Number != 1 || att.Outcome != domain.AttemptUnverified {
		t.Errorf("unexpected attempt: %+v", att)
	}
	if inc.AttemptsRemaining() != 2 {
		t.Errorf("expected 2 attempts remaining, got %d", inc.AttemptsRemaining())
	}

	// Reject requires proposal
	if err := inc.Reject("operator-bob", "bad candidate", now); err == nil {
		t.Error("expected error rejecting incident without proposal")
	}

	// Propose a candidate
	gates := []domain.GateResult{
		{Gate: domain.GateG1Structural, Passed: true, Detail: "well formed", At: now},
		{Gate: domain.GateG2Shape, Passed: true, Detail: "shape matches", At: now},
		{Gate: domain.GateG3Stability, Passed: true, Detail: "stable across replays", At: now},
		{Gate: domain.GateG4Semantic, Passed: true, Detail: "type matches", At: now},
		{Gate: domain.GateG5Continuity, Passed: true, Detail: "continuity intact", At: now},
	}
	candBinding := domain.Binding{
		ID:                "b-prop-1",
		CheckID:           chkID,
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp-new",
		Version:           2,
		Origin:            domain.OriginRepaired,
		Locators: []domain.Locator{
			{Target: "price", Dialect: "css", Expression: ".price-new"},
		},
	}
	prop := domain.RepairProposal{
		Binding:         candBinding,
		VerifiedAgainst: "snap-kg-1",
		VerifiedAt:      now,
		Rationale:       "Element moved from #old-price to .price-new",
		Gates:           gates,
		CandidateNumber: 2,
	}
	if err := prop.Validate(); err != nil {
		t.Fatalf("RepairProposal.Validate failed: %v", err)
	}
	if prop.HumanSummary() == "" {
		t.Error("HumanSummary should not be empty")
	}

	if err := inc.Propose(prop, now.Add(2*time.Second)); err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if inc.State() != domain.IncidentAwaitingApproval {
		t.Errorf("expected IncidentAwaitingApproval, got %s", inc.State())
	}

	// ApproveWithEdits
	editedLocators := []domain.Locator{
		{Target: "price", Dialect: "css", Expression: ".price-custom"},
	}
	if err := inc.ApproveWithEdits("operator-bob", editedLocators, now.Add(3*time.Second)); err != nil {
		t.Fatalf("ApproveWithEdits failed: %v", err)
	}

	appBinding, err := inc.ApprovedBinding()
	if err != nil {
		t.Fatalf("ApprovedBinding failed: %v", err)
	}
	if appBinding.Locators[0].Expression != ".price-custom" {
		t.Errorf("expected edited locator, got %s", appBinding.Locators[0].Expression)
	}

	// ResolveWithRepair
	if err := inc.ResolveWithRepair(now.Add(4*time.Second)); err != nil {
		t.Fatalf("ResolveWithRepair failed: %v", err)
	}
	if inc.State() != domain.IncidentResolved {
		t.Errorf("expected IncidentResolved, got %s", inc.State())
	}
	if err := inc.CheckInvariants(); err != nil {
		t.Errorf("CheckInvariants failed on resolved incident: %v", err)
	}

	// ResolveRecovered
	inc2, _ := domain.NewIncident("inc-cov-2", chkID, f, 3, now)
	if err := inc2.ResolveRecovered(now.Add(time.Hour)); err != nil {
		t.Fatalf("ResolveRecovered failed: %v", err)
	}
	if inc2.State() != domain.IncidentResolved {
		t.Errorf("expected IncidentResolved, got %s", inc2.State())
	}

	// Abandon
	inc3, _ := domain.NewIncident("inc-cov-3", chkID, f, 3, now)
	if err := inc3.Abandon("operator abandoned", now.Add(time.Hour)); err != nil {
		t.Fatalf("Abandon failed: %v", err)
	}
	if inc3.State() != domain.IncidentAbandoned {
		t.Errorf("expected IncidentAbandoned, got %s", inc3.State())
	}
}

func TestDomain_BindingCoversAndValidation(t *testing.T) {
	// Intent Scalar
	intentScalar := domain.ScalarIntent{
		Label:   "price",
		Purpose: "unit price",
		Type:    domain.TypeNumber,
	}
	bScalar := domain.Binding{
		ID:                "b-s-1",
		CheckID:           "chk-1",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp-1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: "price", Dialect: "css", Expression: ".price"},
		},
	}
	if err := bScalar.Covers(intentScalar); err != nil {
		t.Errorf("expected scalar binding to cover intent: %v", err)
	}

	// Intent Record
	intentRecord := domain.RecordIntent{
		Label:   "product",
		Purpose: "product details",
		Fields: []domain.Field{
			{Name: "title", Type: domain.TypeString, Required: true},
			{Name: "price", Type: domain.TypeNumber, Required: true},
			{Name: "stock", Type: domain.TypeNumber, Required: false},
		},
	}
	bRecord := domain.Binding{
		ID:                "b-r-1",
		CheckID:           "chk-1",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentRecord,
		Fingerprint:       "fp-1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: "title", Dialect: "css", Expression: "h1"},
			{Target: "price", Dialect: "css", Expression: ".price"},
		},
	}
	if err := bRecord.Covers(intentRecord); err != nil {
		t.Errorf("expected record binding to cover required fields: %v", err)
	}

	// Missing required field in Record
	bMissing := domain.Binding{
		ID:                "b-r-2",
		CheckID:           "chk-1",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentRecord,
		Fingerprint:       "fp-1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: "stock", Dialect: "css", Expression: ".stock"},
		},
	}
	if err := bMissing.Covers(intentRecord); err == nil {
		t.Error("expected error for missing required title field")
	}

	// Intent Collection
	intentColl := domain.CollectionIntent{
		Label:   "items",
		Purpose: "item list",
		Element: intentRecord,
	}
	bColl := domain.Binding{
		ID:                "b-c-1",
		CheckID:           "chk-1",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentCollection,
		Fingerprint:       "fp-1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: domain.CollectionRoot, Dialect: "css", Expression: "li.item"},
			{Target: "title", Dialect: "css", Expression: "h1"},
			{Target: "price", Dialect: "css", Expression: ".price"},
		},
	}
	if err := bColl.Covers(intentColl); err != nil {
		t.Errorf("expected collection binding to cover collection intent: %v", err)
	}

	// Binding.Trace()
	tr := bColl.Trace()
	if tr.CheckID != "chk-1" || tr.DefinitionVersion != 1 || tr.IntentKind != domain.IntentCollection {
		t.Errorf("unexpected trace: %+v", tr)
	}

	// Stale check
	if !bColl.Stale("fp-changed") {
		t.Error("expected Stale to be true when fingerprint changes")
	}
}

func TestDomain_RunChangedAndInvariants(t *testing.T) {
	now := time.Now().UTC()
	r, err := domain.NewRun("run-changed-1", "chk-1", 10, 1, now)
	if err != nil {
		t.Fatalf("NewRun failed: %v", err)
	}
	_ = r.Start(now, 1)

	res := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "99", Type: domain.TypeNumber}}
	if err := r.Changed(now.Add(time.Second), "snap-changed", res, "price dropped by $10"); err != nil {
		t.Fatalf("Changed failed: %v", err)
	}
	if r.State() != domain.StateChanged {
		t.Errorf("expected StateChanged, got %s", r.State())
	}
	if r.Explanation() != "price dropped by $10" {
		t.Errorf("unexpected explanation: %s", r.Explanation())
	}

	// Check invariants on corrupted runs via RestoreRun
	if _, err := domain.RestoreRun(domain.RestoredRun{
		ID:        "run-bad-inv-1",
		CheckID:   "chk-1",
		State:     domain.StateQuiet,
		CreatedAt: now,
		// EndedAt is zero
	}); err == nil {
		t.Error("expected invariant failure for terminal run with zero endedAt")
	}

	if _, err := domain.RestoreRun(domain.RestoredRun{
		ID:        "run-bad-inv-2",
		CheckID:   "chk-1",
		State:     domain.StateRunning,
		CreatedAt: now,
		// StartedAt is zero
	}); err == nil {
		t.Error("expected invariant failure for running run with zero startedAt")
	}

	if _, err := domain.RestoreRun(domain.RestoredRun{
		ID:        "run-bad-inv-3",
		CheckID:   "chk-1",
		State:     domain.StateFailed,
		CreatedAt: now,
		StartedAt: now,
		EndedAt:   now.Add(time.Second),
	}); err == nil {
		t.Error("expected invariant failure for failed run with nil failure")
	}

	// Snapshot index invariants
	snapIdx := domain.NewSnapshotIndex("chk-snap-inv")
	snap1, _ := domain.NewSnapshot("chk-other", "text/html", []byte("xyz"), "fp", now)
	if err := snapIdx.Add(snap1); err == nil {
		t.Error("expected error adding snapshot belonging to different check")
	}
}

func TestDomain_EdgeCasesAndInvariants(t *testing.T) {
	now := time.Now().UTC()

	// 1. SourceSpec Validation
	sPluginValid := domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "my-plugin"}
	if err := sPluginValid.Validate(); err != nil {
		t.Errorf("expected valid plugin source, got %v", err)
	}
	sPluginInvalid := domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: ""}
	if err := sPluginInvalid.Validate(); err == nil {
		t.Error("expected error for empty plugin name")
	}
	sUnknown := domain.SourceSpec{Kind: "ftp"}
	if err := sUnknown.Validate(); err == nil {
		t.Error("expected error for unknown source kind")
	}

	// 2. Destination Validation
	dNone := domain.Destination{Kind: domain.DestinationNone}
	if err := dNone.Validate(); err != nil {
		t.Errorf("DestinationNone should be valid: %v", err)
	}
	dNotifyValid := domain.Destination{Kind: domain.DestinationNotify, Target: "https://slack.example"}
	if err := dNotifyValid.Validate(); err != nil {
		t.Errorf("DestinationNotify with target should be valid: %v", err)
	}
	dNotifyInvalid := domain.Destination{Kind: domain.DestinationNotify, Target: ""}
	if err := dNotifyInvalid.Validate(); err == nil {
		t.Error("expected error for DestinationNotify without target")
	}
	dUnknown := domain.Destination{Kind: "carrier_pigeon"}
	if err := dUnknown.Validate(); err == nil {
		t.Error("expected error for unknown destination kind")
	}

	// 3. Schedule SlotAt edge cases
	sZero := domain.Schedule{Interval: 0}
	if sZero.SlotAt(now) != 0 {
		t.Errorf("expected slot 0 for zero interval")
	}

	// 4. Policy zero values
	pZero := domain.Policy{}
	if pZero.Attempts() != domain.DefaultMaxRepairAttempts {
		t.Errorf("expected default repair attempts, got %d", pZero.Attempts())
	}
	if pZero.Retries() != domain.DefaultMaxRetries {
		t.Errorf("expected default retries, got %d", pZero.Retries())
	}

	// 5. Incident invariants and edge cases
	if domain.IncidentState("bogus").Valid() {
		t.Error("bogus incident state should not be valid")
	}
	if domain.Gate("bogus").Valid() {
		t.Error("bogus gate should not be valid")
	}

	// RestoreIncident validation
	if _, err := domain.RestoreIncident(domain.RestoredIncident{}); err == nil {
		t.Error("expected error restoring incident with empty ID")
	}
	if _, err := domain.RestoreIncident(domain.RestoredIncident{
		ID:    "inc-bad-state",
		State: "invalid_state",
	}); err == nil {
		t.Error("expected error restoring incident with invalid state")
	}

	// Snapshot Verify edge cases: RestoreSnapshot verifies ID == sha256(Body)
	if _, err := domain.RestoreSnapshot(domain.RestoredSnapshot{
		ID:          "snap-bad",
		CheckID:     "chk-1",
		ContentType: "text/html",
		Body:        []byte("abc"),
		Fingerprint: "fp-1",
		CapturedAt:  now,
	}); err == nil {
		t.Error("expected error restoring snapshot when ID does not match sha256 of body")
	}

	validID := domain.SnapshotID("sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	sValid, err := domain.RestoreSnapshot(domain.RestoredSnapshot{
		ID:          validID,
		CheckID:     "chk-1",
		ContentType: "text/html",
		Body:        []byte("abc"),
		Fingerprint: "fp-1",
		CapturedAt:  now,
	})
	if err != nil {
		t.Fatalf("expected valid RestoreSnapshot: %v", err)
	}
	if err := sValid.Verify(); err != nil {
		t.Fatalf("expected sValid.Verify to pass: %v", err)
	}

	// SnapshotIndex CheckInvariants with everHadKnownGood = true but none present
	idxEmpty := domain.NewSnapshotIndex("chk-empty")
	if err := idxEmpty.CheckInvariants(true); err == nil {
		t.Error("expected error when everHadKnownGood is true but none in index")
	}

	// ExpiredSnapshots(nil)
	ret := domain.DefaultRetention()
	if exp := ret.ExpiredSnapshots(nil); exp != nil {
		t.Errorf("expected nil for ExpiredSnapshots(nil), got %v", exp)
	}

	// NewRun with empty ID
	if _, err := domain.NewRun("", "chk-1", 1, 1, now); err == nil {
		t.Error("expected error creating run with empty ID")
	}
	if _, err := domain.NewRun("run-1", "", 1, 1, now); err == nil {
		t.Error("expected error creating run with empty CheckID")
	}
}

func TestDomain_ExtractionValidationAndIncidentInvariants(t *testing.T) {
	now := time.Now().UTC()

	// 1. Extraction.Validate
	eScalarBad := domain.Extraction{
		Kind:   domain.IntentScalar,
		Record: domain.Record{"a": domain.Value{Text: "1"}},
	}
	if err := eScalarBad.Validate(); err == nil {
		t.Error("expected error for scalar extraction carrying record data")
	}

	eRecordBadColl := domain.Extraction{
		Kind:       domain.IntentRecord,
		Record:     domain.Record{"a": domain.Value{Text: "1"}},
		Collection: []domain.Record{{"a": domain.Value{Text: "1"}}},
	}
	if err := eRecordBadColl.Validate(); err == nil {
		t.Error("expected error for record extraction carrying collection data")
	}

	eRecordNil := domain.Extraction{
		Kind:   domain.IntentRecord,
		Record: nil,
	}
	if err := eRecordNil.Validate(); err == nil {
		t.Error("expected error for record extraction with nil record")
	}

	eCollBadRec := domain.Extraction{
		Kind:       domain.IntentCollection,
		Record:     domain.Record{"a": domain.Value{Text: "1"}},
		Collection: []domain.Record{{"a": domain.Value{Text: "1"}}},
	}
	if err := eCollBadRec.Validate(); err == nil {
		t.Error("expected error for collection extraction carrying single record")
	}

	eUnknown := domain.Extraction{
		Kind: domain.IntentKind("matrix"),
	}
	if err := eUnknown.Validate(); err == nil {
		t.Error("expected error for unknown extraction kind")
	}

	// 2. Incident Invariants via RestoreIncident
	f := domain.Failure{Class: domain.ClassStructural, Code: "e", Summary: "broken"}

	// attempts out of order
	_, err := domain.RestoreIncident(domain.RestoredIncident{
		ID:        "inc-bad-att-order",
		CheckID:   "chk-1",
		Cause:     f,
		MaxAttempts: 3,
		State:     domain.IncidentOpen,
		OpenedAt:  now,
		Attempts: []domain.RepairAttempt{
			{Number: 2, At: now, Outcome: domain.AttemptUnverified},
		},
	})
	if err == nil {
		t.Error("expected error restoring incident with out of order attempts")
	}

	// attempts exceeding budget
	_, err = domain.RestoreIncident(domain.RestoredIncident{
		ID:        "inc-bad-att-budget",
		CheckID:   "chk-1",
		Cause:     f,
		MaxAttempts: 1,
		State:     domain.IncidentOpen,
		OpenedAt:  now,
		Attempts: []domain.RepairAttempt{
			{Number: 1, At: now, Outcome: domain.AttemptUnverified},
			{Number: 2, At: now, Outcome: domain.AttemptUnverified},
		},
	})
	if err == nil {
		t.Error("expected error restoring incident with attempts exceeding budget")
	}

	// closed without close time
	_, err = domain.RestoreIncident(domain.RestoredIncident{
		ID:        "inc-bad-close-time",
		CheckID:   "chk-1",
		Cause:     f,
		MaxAttempts: 3,
		State:     domain.IncidentResolved,
		OpenedAt:  now,
		// ClosedAt is zero
	})
	if err == nil {
		t.Error("expected error restoring resolved incident with zero closedAt")
	}

	// awaiting approval without proposal
	_, err = domain.RestoreIncident(domain.RestoredIncident{
		ID:        "inc-bad-awaiting-prop",
		CheckID:   "chk-1",
		Cause:     f,
		MaxAttempts: 3,
		State:     domain.IncidentAwaitingApproval,
		OpenedAt:  now,
		Proposal:  nil,
	})
	if err == nil {
		t.Error("expected error restoring awaiting approval incident without proposal")
	}

	// 3. Audit validation
	badAuditActor := domain.AuditEvent{
		ID:          "ev-bad-actor",
		Action:      domain.ActionCheckCreated,
		SubjectKind: domain.SubjectCheck,
		SubjectID:   "chk-1",
		At:          now,
		// Actor empty
	}
	if err := badAuditActor.Validate(); err == nil {
		t.Error("expected error for empty actor in audit")
	}

	badAuditSubject := domain.AuditEvent{
		ID:     "ev-bad-subj",
		Action: domain.ActionCheckCreated,
		Actor:  "alice",
		At:     now,
		// Subject empty
	}
	if err := badAuditSubject.Validate(); err == nil {
		t.Error("expected error for empty subject in audit")
	}

	badAuditTime := domain.AuditEvent{
		ID:          "ev-bad-time",
		Action:      domain.ActionCheckCreated,
		Actor:       "alice",
		SubjectKind: domain.SubjectCheck,
		SubjectID:   "chk-1",
		// At zero
	}
	if err := badAuditTime.Validate(); err == nil {
		t.Error("expected error for zero time in audit")
	}
}




