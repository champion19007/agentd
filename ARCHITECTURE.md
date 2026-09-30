# Agentd Architecture Specification

Agentd is architected as a **hexagonal modular monolith** in pure Go, with strict boundary enforcement, pure deterministic core logic, and single-writer persistence.

---

## 1. Hexagonal Dependency Flow

Dependencies point inward only:

```text
driving adapters   -->   core   -->   ports   <--   driven adapters
(CLI, HTTP API)       (domain)     (interfaces)     (SQLite, HTTP, Model, Plugins)
```

| Component | Directory | Responsibility |
| :--- | :--- | :--- |
| **Composition Root** | `cmd/agentd/` | Instantiates adapters, injects dependencies through ports, manages signal handling. Contains no business rules. |
| **Core Domain** | `internal/core/domain/` | Entity aggregates (`Check`, `Run`, `Incident`, `Snapshot`, `Binding`), value types, and invariants. |
| **Core Scheduling** | `internal/core/scheduling/` | Deterministic slot arithmetic, catch-up calculations, and dispatch planning. |
| **Core Run Engine** | `internal/core/run/` | Run state machine, worker pool, hash gating, and crash recovery. |
| **Core Repair** | `internal/core/repair/` | Multi-gate verification (G1–G5), candidate evaluation, and approval workflow. |
| **Ports** | `internal/ports/` | Interfaces declared by the core and implemented by outside adapters. |
| **Store Adapter** | `internal/store/sqlite/` | SQLite WAL persistence, zstd compression, hash deduplication, hot backups. |
| **Source Adapters** | `internal/adapters/` | SSRF-hardened HTTP client, HTML extractor, BYOK model client, notification spool. |
| **Plugin Host** | `internal/plugins/` | Model Context Protocol (MCP) child process sandboxing over stdio. |

---

## 2. Invariants Enforced by Architecture Tests

`tests/architecture_test.go` automatically inspects the Go AST to ensure that code inside `internal/core/...`:
1. **Never imports I/O packages**: No `net`, `net/http`, `os`, `os/exec`, `io/ioutil`, `database/sql`, `modernc.org/sqlite`.
2. **Never imports adapters**: No importing `internal/adapters`, `internal/api`, `internal/cli`, `internal/store`, `internal/plugins`.
3. **Never reads the wall clock**: No calling `time.Now()`, `time.Sleep()`, `time.After()`, `time.NewTicker()`, `time.Since()`. All instants and durations arrive via `ports.Clock` or are passed directly as arguments.

---

## 3. Scheduling & Deterministic Slots

Slots eliminate wall-clock race conditions and timing ambiguities:

$$\text{Slot} = \left\lfloor \frac{\text{Instant}}{\text{Interval}} \right\rfloor$$

Because a slot is a pure integer value, the database enforces the statement **"at most one run per (check, slot)"** via a SQLite `UNIQUE(check_id, slot)` constraint. Two concurrent schedulers or worker threads cannot duplicate work.

---

## 4. Single-Writer SQLite Concurrency

Agentd avoids `SQLITE_BUSY` errors by architectural design rather than retrying and hoping:

- **Writer Pool (`MaxOpenConns = 1`)**: All mutations (inserts, updates, transactions) serialize through a single dedicated write connection. Contention is resolved inside Go's cancellable connection pool queue, never colliding inside SQLite.
- **Reader Pool (`MaxOpenConns = N`)**: Read operations execute across multiple concurrent connections in SQLite WAL mode. Readers never block writers, and writers never block readers.
- **Zstandard Compression**: Raw HTML snapshots are compressed with `klauspost/compress/zstd` before disk storage, reducing database size by up to 85%.

---

## 5. Conservative Healing Subsystem

```text
Extraction Failure
       │
       ▼
Structural Classification
       │
       ▼
Incident Opened (Attempt 1 of 3)
       │
       ▼
Candidate Binding Generation (LLM)
       │
       ▼
Verification Gates (G1 to G5)
       │
       ├── Fail ──> Record attempt failure (up to 3)
       ▼ Pass
Human-Readable Repair Proposal Generated
       │
       ▼
Human Operator Approval (agentd repair approve --by ...)
       │
       ▼
Binding Activated & Persisted
```

### Verification Gates (G1–G5)
Every candidate locator must satisfy all 5 gates before being presented to the operator:
- **G1 (Syntax & Parsing)**: Valid locator syntax (valid CSS selector / JSONPath).
- **G2 (Non-Empty Extraction)**: Locates non-empty text when applied against the current source snapshot.
- **G3 (Schema Conformance)**: Extracted value conforms to the intent type (`string`, `number`, `bool`, `timestamp`).
- **G4 (Known-Good Consistency)**: Successfully extracts expected historical data when applied against the check's historical known-good snapshot.
- **G5 (Disambiguation)**: Selector matches exactly one DOM target, avoiding ambiguous multi-node matches.
