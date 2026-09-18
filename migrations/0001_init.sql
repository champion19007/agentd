-- 0001_init: the initial Agentd schema.
--
-- Conventions used throughout:
--
--   tenant_id leads every primary key and every composite index. Agentd v1 is
--   single-tenant and writes a constant value, but a leading tenant_id means
--   the indexes that serve today's queries also serve per-tenant ones later.
--   Adding the column afterwards would mean rebuilding every index in the
--   database.
--
--   Instants are RFC3339 text in UTC, which sorts lexicographically in the
--   same order it sorts chronologically. A nullable instant means the stage it
--   describes was never reached.
--
--   Structured values the core owns -- intents, locators, failures, results --
--   are stored as JSON rather than shredded into columns. They are read and
--   written whole, never queried into, and giving them columns would couple
--   the schema to domain shapes that are still settling.
--
--   STRICT tables, so a type error is a write failure rather than a surprise
--   three months later.

-- checks ---------------------------------------------------------------------

CREATE TABLE checks (
    tenant_id      TEXT    NOT NULL,
    id             TEXT    NOT NULL,
    enabled        INTEGER NOT NULL,
    active_version INTEGER NOT NULL,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, id)
) STRICT;

CREATE INDEX checks_enabled_ix ON checks (tenant_id, enabled);

-- check_versions holds every definition a check has ever had. Old versions are
-- kept because runs reference the version they ran under, and an explanation of
-- a past run has to stay truthful after the check is edited.
CREATE TABLE check_versions (
    tenant_id        TEXT    NOT NULL,
    check_id         TEXT    NOT NULL,
    version          INTEGER NOT NULL,
    intent_kind      TEXT    NOT NULL,
    intent_json      TEXT    NOT NULL,
    source_json      TEXT    NOT NULL,
    schedule_json    TEXT    NOT NULL,
    destination_json TEXT    NOT NULL,
    policy_json      TEXT    NOT NULL,
    created_at       TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, check_id, version),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
) STRICT;

-- bindings -------------------------------------------------------------------

-- A binding is derived, never authored. Note there is no path from this table
-- to a check definition: the separation that makes repair verifiable is a fact
-- about the schema, not only about the Go types.
CREATE TABLE bindings (
    tenant_id          TEXT    NOT NULL,
    id                 TEXT    NOT NULL,
    check_id           TEXT    NOT NULL,
    definition_version INTEGER NOT NULL,
    intent_kind        TEXT    NOT NULL,
    fingerprint        TEXT    NOT NULL,
    version            INTEGER NOT NULL,
    origin             TEXT    NOT NULL,
    locators_json      TEXT    NOT NULL,
    derived_at         TEXT    NOT NULL,
    derived_from       TEXT,
    active             INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX bindings_version_uk ON bindings (tenant_id, check_id, version);

-- At most one binding is in force per check, enforced here rather than only in
-- application code so that two processes activating at once cannot both win.
CREATE UNIQUE INDEX bindings_active_uk ON bindings (tenant_id, check_id) WHERE active = 1;

-- runs -----------------------------------------------------------------------

CREATE TABLE runs (
    tenant_id          TEXT    NOT NULL,
    id                 TEXT    NOT NULL,
    check_id           TEXT    NOT NULL,
    slot               INTEGER NOT NULL,
    definition_version INTEGER NOT NULL,
    binding_version    INTEGER NOT NULL DEFAULT 0,
    state              TEXT    NOT NULL,
    created_at         TEXT    NOT NULL,
    started_at         TEXT,
    ended_at           TEXT,
    snapshot_id        TEXT,
    result_json        TEXT,
    failure_json       TEXT,
    explanation        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
) STRICT;

-- One run per (check, slot). This is the database's job, not the
-- application's: an application check can only report what was true a moment
-- ago, and two runners racing for the same slot would both pass it.
CREATE UNIQUE INDEX runs_slot_uk ON runs (tenant_id, check_id, slot);

CREATE INDEX runs_recent_ix ON runs (tenant_id, check_id, created_at DESC);

-- Serves retention sweeps and the "what is the last thing this check
-- extracted" lookup.
CREATE INDEX runs_state_ix ON runs (tenant_id, check_id, state, ended_at DESC);

-- snapshots ------------------------------------------------------------------

-- Bodies are content addressed: id is the sha256 of the uncompressed bytes.
-- Storing the same capture twice for one check is therefore impossible, which
-- is the deduplication the domain's SnapshotIndex assumes.
--
-- The key includes check_id because retention and the known-good flag are
-- per-check properties. Two checks that happen to observe identical bytes each
-- keep a copy; sharing one blob between them is a later optimisation and would
-- need its own reference counting to retain safely.
CREATE TABLE snapshots (
    tenant_id    TEXT    NOT NULL,
    check_id     TEXT    NOT NULL,
    id           TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    size_bytes   INTEGER NOT NULL,
    fingerprint  TEXT    NOT NULL,
    captured_at  TEXT    NOT NULL,
    known_good   INTEGER NOT NULL DEFAULT 0,
    compression  TEXT    NOT NULL,
    body         BLOB    NOT NULL,
    PRIMARY KEY (tenant_id, check_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE INDEX snapshots_retention_ix ON snapshots (tenant_id, check_id, known_good, captured_at DESC);

-- A run records only the content address of its capture, so fetching one by
-- address alone has to be an index lookup rather than a scan of every check.
CREATE INDEX snapshots_id_ix ON snapshots (tenant_id, id);

-- incidents ------------------------------------------------------------------

CREATE TABLE incidents (
    tenant_id    TEXT    NOT NULL,
    id           TEXT    NOT NULL,
    check_id     TEXT    NOT NULL,
    state        TEXT    NOT NULL,
    cause_json   TEXT    NOT NULL,
    opened_at    TEXT    NOT NULL,
    closed_at    TEXT,
    max_attempts INTEGER NOT NULL,
    resolution   TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE
) STRICT;

-- At most one open incident per check. A check failing every five minutes must
-- not be able to open an incident every five minutes, and two processes
-- noticing the same breakage simultaneously must not both succeed.
CREATE UNIQUE INDEX incidents_open_uk ON incidents (tenant_id, check_id)
    WHERE state IN ('open', 'awaiting_approval');

CREATE INDEX incidents_closed_ix ON incidents (tenant_id, closed_at);

-- repair ---------------------------------------------------------------------

-- One row per attempt, whether or not it worked. The failures are the point:
-- an operator deciding whether to trust a proposal wants to see what else was
-- tried on their behalf.
CREATE TABLE repair_candidates (
    tenant_id      TEXT    NOT NULL,
    id             TEXT    NOT NULL,
    incident_id    TEXT    NOT NULL,
    check_id       TEXT    NOT NULL,
    attempt_number INTEGER NOT NULL,
    outcome        TEXT    NOT NULL,
    binding_id     TEXT,
    locators_json  TEXT    NOT NULL DEFAULT '[]',
    rationale      TEXT    NOT NULL DEFAULT '',
    note           TEXT    NOT NULL DEFAULT '',
    -- Which stored capture this candidate was replayed against, and when. This
    -- is the current capture, not the known-good one: a repair is derived for
    -- the shape the source has now.
    verified_against TEXT,
    verified_at      TEXT,
    created_at     TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX repair_candidates_attempt_uk
    ON repair_candidates (tenant_id, incident_id, attempt_number);

-- Each gate a candidate had to pass, recorded separately so that "it failed
-- verification" can always be answered with which check failed and why.
CREATE TABLE verification_results (
    tenant_id       TEXT    NOT NULL,
    id              TEXT    NOT NULL,
    incident_id     TEXT    NOT NULL,
    candidate_id    TEXT    NOT NULL,
    gate            TEXT    NOT NULL,
    passed          INTEGER NOT NULL,
    detail          TEXT    NOT NULL DEFAULT '',
    verified_against TEXT   NOT NULL,
    verified_at     TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, candidate_id) REFERENCES repair_candidates (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE INDEX verification_results_candidate_ix
    ON verification_results (tenant_id, candidate_id, verified_at);

-- The human decision. decided_by is NOT NULL and is required to be non-empty
-- by the application: an approval with nobody attached to it is
-- indistinguishable from Agentd approving its own work.
CREATE TABLE repair_decisions (
    tenant_id    TEXT NOT NULL,
    id           TEXT NOT NULL,
    incident_id  TEXT NOT NULL,
    candidate_id TEXT NOT NULL,
    decision     TEXT NOT NULL,
    decided_by   TEXT NOT NULL,
    decided_at   TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE INDEX repair_decisions_incident_ix ON repair_decisions (tenant_id, incident_id, decided_at);

-- When an operator edits a proposal before approving it, both sides are kept.
-- What Agentd suggested and what the human actually approved are different
-- facts, and collapsing them would lose the record of Agentd having been
-- partly wrong.
CREATE TABLE repair_diffs (
    tenant_id              TEXT NOT NULL,
    id                     TEXT NOT NULL,
    incident_id            TEXT NOT NULL,
    candidate_id           TEXT NOT NULL,
    proposed_locators_json TEXT NOT NULL,
    approved_locators_json TEXT NOT NULL,
    edited_by              TEXT NOT NULL,
    edited_at              TEXT NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id) ON DELETE CASCADE
) STRICT;

-- audit ----------------------------------------------------------------------

-- Everything Agentd did and everything a human told it to do. This table is
-- append-only by convention and is deliberately not covered by the retention
-- sweep: the whole point of a tool that asks permission is being able to show,
-- later, exactly what was asked and what was answered.
CREATE TABLE audit_events (
    tenant_id    TEXT NOT NULL,
    id           TEXT NOT NULL,
    at           TEXT NOT NULL,
    actor        TEXT NOT NULL,
    action       TEXT NOT NULL,
    subject_kind TEXT NOT NULL,
    subject_id   TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
) STRICT;

CREATE INDEX audit_events_time_ix ON audit_events (tenant_id, at DESC);
CREATE INDEX audit_events_subject_ix ON audit_events (tenant_id, subject_kind, subject_id, at DESC);
