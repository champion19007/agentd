# Agentd Migrations Guide

This guide details schema evolution for both **Job Configurations** and the underlying **SQLite Database Store**.

---

## 1. Job Schema Versioning & Forward Migration

Agentd follows a strict **N and N-1** compatibility policy for check definitions:
- **Current Version**: Schema v2 (Structured domain model).
- **Supported Previous Version**: Schema v1 (Flat legacy format).

### Forward Migration on Load
When Agentd loads a job configuration from disk (`agentd check add --file check.json` or through internal configuration loading), it determines the schema version:

1. **If Version == 1**: Agentd parses the v1 fields and automatically converts them in-memory to the canonical v2 representation, setting default values for missing fields (e.g. `catch_up: once`, `dialect: css`, `method: GET`).
2. **If Version == 2**: Loaded natively.
3. **If Version > 2**: Rejected with `ErrFutureVersion` to protect against forward incompatibilities when running older binaries against newer configurations.
4. **If Version < 1**: Rejected with `ErrUnsupportedVersion`.

### Write Migrated Form on Edit
Whenever an operator modifies, exports, or updates a check (`agentd check export <id>`), Agentd **always serializes the current v2 schema format**. Legacy v1 formats are transparently phased out without operator intervention.

---

## 2. SQLite Database Sequential Migrations

The persistence store uses sequential, forward-only SQL migrations embedded directly inside the binary via `embed.FS` ([`migrations/`](migrations/)):

```text
migrations/
├── 0001_init.sql                  # Core tables (checks, runs, incidents, snapshots)
├── 0002_global_snapshot_dedup.sql # Global SHA-256 snapshot content-addressed deduplication
└── embed.go                       # Embedded filesystem bundle
```

### Automatic Migration on Startup
When `agentd` initializes or opens a database file, the migration runner:
1. Creates the `schema_migrations` tracking table if it does not already exist.
2. Identifies which migrations have already been applied.
3. Applies any pending numbered migrations sequentially in discrete transactions (`tx.BeginTx`).
4. Rejects out-of-order migration gaps (detecting conflicting branches).

---

## 3. Major Migration Safety & Backup Recommendations

### Automated Backup Warning
When Agentd detects that an unapplied migration contains a major or breaking schema transition on an **existing production database** (where previous migrations have already run), Agentd emits an explicit warning to `stderr`:

```text
agentd: [BACKUP RECOMMENDED] Applying major schema migration 0003 (major_incident_graph).
It is strongly recommended to run 'agentd backup --to <path> --verify' before applying major migrations to an existing database.
```

Operators can inspect pending migrations programmatically using `store.PendingMigrations(ctx)`.

---

## 4. Rollback Strategy & Disaster Recovery

### Why Down Migrations Do Not Exist
Agentd **deliberately does not provide downward (`DOWN`) SQL migrations**.

Running reverse migrations against production data is a high-risk, rarely tested code path executed under crisis conditions. In practice, downward migrations frequently result in data corruption or irreversible data loss.

### The Safe Rollback Protocol
To roll back a major version upgrade:
1. **Always take a verified hot backup before upgrading**:
   ```bash
   agentd backup --to /backups/agentd-pre-v1.db --verify
   ```
2. If an upgrade must be reverted:
   - Terminate the new daemon.
   - Restore the verified pre-migration backup file:
     ```bash
     cp /backups/agentd-pre-v1.db /var/lib/agentd/agentd.db
     ```
   - Restart the previous binary version.
