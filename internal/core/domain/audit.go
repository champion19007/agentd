package domain

import "time"

// The audit trail is the receipt for a tool that asks permission.
//
// Agentd's central claim is that it never changes anything without being told
// to. A claim like that is only worth something if it can be checked
// afterwards, by someone who was not watching at the time. So every decision
// that mattered -- what broke, what Agentd proposed, who said yes, what was
// actually applied -- leaves a row, written in the same transaction as the
// thing it describes.
//
// Audit events are never swept by retention. They are the smallest and
// longest-lived thing in the database, and the one an operator will want years
// after the run it refers to has gone.

// ActorAgentd is the actor for something Agentd did by itself. Anything else
// is a person's identifier, and the distinction is the point of the field.
const ActorAgentd = "agentd"

// AuditAction names what happened.
type AuditAction string

const (
	ActionCheckCreated     AuditAction = "check_created"
	ActionCheckRevised     AuditAction = "check_revised"
	ActionCheckEnabled     AuditAction = "check_enabled"
	ActionCheckDisabled    AuditAction = "check_disabled"
	ActionRunRecorded      AuditAction = "run_recorded"
	ActionIncidentOpened   AuditAction = "incident_opened"
	ActionIncidentClosed   AuditAction = "incident_closed"
	ActionRepairAttempted  AuditAction = "repair_attempted"
	ActionRepairProposed   AuditAction = "repair_proposed"
	ActionRepairApproved   AuditAction = "repair_approved"
	ActionRepairEdited     AuditAction = "repair_edited"
	ActionRepairRejected   AuditAction = "repair_rejected"
	ActionBindingActivated AuditAction = "binding_activated"
	ActionNotified         AuditAction = "notified"
	ActionRetentionSwept   AuditAction = "retention_swept"
	ActionBackupTaken      AuditAction = "backup_taken"
)

// Valid reports whether a is a known action.
func (a AuditAction) Valid() bool {
	switch a {
	case ActionCheckCreated, ActionCheckRevised, ActionCheckEnabled, ActionCheckDisabled,
		ActionRunRecorded, ActionIncidentOpened, ActionIncidentClosed,
		ActionRepairAttempted, ActionRepairProposed, ActionRepairApproved,
		ActionRepairEdited, ActionRepairRejected, ActionBindingActivated,
		ActionNotified, ActionRetentionSwept, ActionBackupTaken:
		return true
	}
	return false
}

// RequiresHuman reports whether this action is one only a person can take.
// An event with one of these actions and ActorAgentd as its actor is a bug in
// Agentd worth finding, and AuditEvent.Validate refuses it.
func (a AuditAction) RequiresHuman() bool {
	switch a {
	case ActionRepairApproved, ActionRepairRejected, ActionRepairEdited:
		return true
	}
	return false
}

// SubjectKind names what an event is about.
type SubjectKind string

const (
	SubjectCheck    SubjectKind = "check"
	SubjectRun      SubjectKind = "run"
	SubjectIncident SubjectKind = "incident"
	SubjectBinding  SubjectKind = "binding"
	SubjectDatabase SubjectKind = "database"
)

// AuditEvent is one thing that happened.
type AuditEvent struct {
	// ID identifies the event.
	ID string

	// At is when it happened, supplied by the caller.
	At time.Time

	// Actor is who did it: a person's identifier, or ActorAgentd.
	Actor string

	// Action is what they did.
	Action AuditAction

	// SubjectKind and SubjectID say what it was done to.
	SubjectKind SubjectKind
	SubjectID   string

	// Detail is a sentence in the operator's terms. It is what someone reads
	// when the action name alone does not answer their question.
	Detail string
}

// Validate reports whether e is a usable record.
func (e AuditEvent) Validate() error {
	if !nonEmpty(e.ID) {
		return invalidf("audit event needs an id")
	}
	if !e.Action.Valid() {
		return invalidf("audit event %q has unknown action %q", e.ID, e.Action)
	}
	if !nonEmpty(e.Actor) {
		return invalidf("audit event %q has no actor; an unattributed record proves nothing", e.ID)
	}
	if e.Action.RequiresHuman() && e.Actor == ActorAgentd {
		return invalidf("audit event %q records %q with Agentd as the actor; only a person can do that", e.ID, e.Action)
	}
	if !nonEmpty(string(e.SubjectKind)) || !nonEmpty(e.SubjectID) {
		return invalidf("audit event %q does not say what it is about", e.ID)
	}
	if e.At.IsZero() {
		return invalidf("audit event %q has no time", e.ID)
	}
	return nil
}

// Audit builds an event attributed to Agentd.
func Audit(id string, at time.Time, action AuditAction, kind SubjectKind, subject, detail string) AuditEvent {
	return AuditEvent{
		ID: id, At: at.UTC(), Actor: ActorAgentd, Action: action,
		SubjectKind: kind, SubjectID: subject, Detail: detail,
	}
}

// AuditBy builds an event attributed to a person.
func AuditBy(id string, at time.Time, actor string, action AuditAction, kind SubjectKind, subject, detail string) AuditEvent {
	return AuditEvent{
		ID: id, At: at.UTC(), Actor: actor, Action: action,
		SubjectKind: kind, SubjectID: subject, Detail: detail,
	}
}
