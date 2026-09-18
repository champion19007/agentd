-- 0002: deduplicate snapshot bodies across checks, and give verification gates
-- and edited repairs somewhere to live.
--
-- 0001 keyed snapshots on (tenant_id, check_id, id), so two checks observing
-- identical bytes each kept a copy. That is fine until a handful of checks
-- watch the same status page, at which point the database is mostly duplicate
-- copies of the same HTML.
--
-- Bodies now live once per tenant in snapshot_blobs, and snapshots becomes a
-- per-check reference to one. Retention and the known-good flag stay per check,
-- because they always were per-check properties; only the bytes are shared.
--
-- Sharing means deletion needs counting. A check dropping its last reference to
-- a body must not take the body away from another check still pointing at it,
-- so blobs are removed only when no reference remains -- which the application
-- does explicitly rather than by trigger, so that deletion stays a visible
-- operation rather than a side effect.

-- The shared bodies.
CREATE TABLE snapshot_blobs (
    tenant_id   TEXT    NOT NULL,
    id          TEXT    NOT NULL,   -- sha256 of the uncompressed bytes
    size_bytes  INTEGER NOT NULL,
    compression TEXT    NOT NULL,
    body        BLOB    NOT NULL,
    PRIMARY KEY (tenant_id, id)
) STRICT;

-- Per-check references to a body. content_type, fingerprint and captured_at
-- stay here: the same bytes served under a different content type, or observed
-- at a different moment, are different observations even though they are the
-- same bytes.
CREATE TABLE snapshot_refs (
    tenant_id    TEXT    NOT NULL,
    check_id     TEXT    NOT NULL,
    id           TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    fingerprint  TEXT    NOT NULL,
    captured_at  TEXT    NOT NULL,
    known_good   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, check_id, id),
    FOREIGN KEY (tenant_id, check_id) REFERENCES checks (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, id) REFERENCES snapshot_blobs (tenant_id, id)
) STRICT;

CREATE INDEX snapshot_refs_retention_ix
    ON snapshot_refs (tenant_id, check_id, known_good, captured_at DESC);

-- Counting references for a body, which is what makes deletion safe.
CREATE INDEX snapshot_refs_blob_ix ON snapshot_refs (tenant_id, id);

-- Carry existing captures across. INSERT OR IGNORE collapses what were
-- duplicates into one blob, which is the point of the migration.
INSERT OR IGNORE INTO snapshot_blobs (tenant_id, id, size_bytes, compression, body)
SELECT tenant_id, id, size_bytes, compression, body FROM snapshots;

INSERT OR IGNORE INTO snapshot_refs
    (tenant_id, check_id, id, content_type, fingerprint, captured_at, known_good)
SELECT tenant_id, check_id, id, content_type, fingerprint, captured_at, known_good
  FROM snapshots;

DROP TABLE snapshots;

-- Verification gates -----------------------------------------------------------
--
-- 0001 created verification_results with one row per candidate. Gates are now
-- recorded individually, so a candidate has several rows and the pair
-- (candidate, gate) is what must be unique.

DROP TABLE verification_results;

CREATE TABLE verification_results (
    tenant_id        TEXT    NOT NULL,
    id               TEXT    NOT NULL,
    incident_id      TEXT    NOT NULL,
    candidate_id     TEXT    NOT NULL,
    gate             TEXT    NOT NULL,
    passed           INTEGER NOT NULL,
    detail           TEXT    NOT NULL DEFAULT '',
    verified_against TEXT    NOT NULL DEFAULT '',
    verified_at      TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES incidents (tenant_id, id) ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX verification_results_gate_uk
    ON verification_results (tenant_id, candidate_id, gate);

CREATE INDEX verification_results_incident_ix
    ON verification_results (tenant_id, incident_id, verified_at);

-- Edited repairs ---------------------------------------------------------------
--
-- repair_diffs already existed. It gains nothing structural here, but the
-- application now writes it from the domain rather than only on request, so the
-- index that makes "was this proposal edited" answerable is added.

CREATE INDEX repair_diffs_incident_ix ON repair_diffs (tenant_id, incident_id, edited_at DESC);
