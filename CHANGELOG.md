# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.0.0] - 2026-09-30

### Initial General Availability Release

Agentd is an autonomous web change monitoring daemon designed for high-reliability background observation, deterministic extraction, model-assisted triage, and human-verified repair. Built as a single static binary with no external runtime dependencies or database servers.

#### Major Capabilities
- **Autonomous Monitoring**: Continuous HTTP check evaluation with deterministic interval scheduling and jitter.
- **Intent / Binding Separation**: Check definitions declare semantic extraction intents (`ScalarIntent`, `RecordIntent`, `CollectionIntent`), strictly isolated from fragile DOM locator implementations.
- **Deterministic Extraction**: Pure CSS locator extraction engine with HTML sanitation and typed value parsing (`text`, `number`, `boolean`, `currency`).
- **Model-Assisted Triage & Classification**: Multilateral classification separating true content changes from layout breakage, noise, and transient network errors.
- **Hash-Gated Model Invocation**: Multi-tier hashing (canonical DOM hash, raw payload hash) suppressing 100% of LLM calls on unchanged content.
- **Verified Repair Subsystem**: Multi-gate repair candidate generation:
  - Gate 1: Structural syntax and pseudoprotocol validation.
  - Gate 2: Shape and type consistency with defined intent.
  - Gate 3: Temporal stability across historical runs.
  - Gate 4: Independent semantic verification prompt (model cannot self-verify).
  - Gate 5: Continuity of historical field relationships.
- **Mandatory Human Approval**: Strict "Model proposes, human decides" invariant (`--by` flag required). Repairs are never automatically applied.
- **SQLite Concurrency & Deduplication**: High-concurrency WAL mode (`max_open_conns=1` writer, multi-connection reader pool), single-file `VACUUM INTO` live backups, content-addressed zstd snapshot compression, and transactional state updates.
- **Local HTTP API & MCP Server**:
  - RESTful loopback API (`/v1/...`) with strict cross-origin browser protection.
  - Model Context Protocol (MCP) server over stdio and HTTP POST (`/v1/mcp`) with 7 read-only inspection tools.
- **Extensible Plugin Engine**: Subprocess-isolated JSON-RPC scraper and source plugin architecture with automatic heartbeat liveness, crash restart backoff, and process tree isolation.
- **Observability**: Prometheus `/metrics` exposition, run-level correlation trace IDs, and append-only cryptographic audit trail.

#### Security & Hardening Highlights (Audit Closed)
- **SEC-01**: Loopback HTTP API cross-origin protection (rejects `Sec-Fetch-Site: cross-site`, verifies `Origin` is loopback, rejects `Origin: null`, enforces loopback `Host` header against DNS rebinding).
- **SEC-02**: Repair locator pseudoprotocol sanitization (blocks `javascript:`, `data:`, `vbscript:`, `file:`, `exec:`, and shell chaining characters with whitespace-stripping evasion protection).
- **SEC-03**: 4 MiB `http.MaxBytesReader` request body ceiling on API mutation endpoints.
- **SEC-04**: Dedicated 10s TCP connect timeout separated from 30s HTTP fetch timeout.
- **SEC-05**: Clamped Prometheus check ID metric labels (128 chars maximum).
- **SEC-06**: Query result limit bounds (10,000 maximum) on audit log queries.
- **SEC-07**: Windows subprocess tree cleanup using background `taskkill.exe /T /F` on session termination.
- **SEC-08**: Documented forward-only migration semantics from v1 to v2 job configuration schemas.
- **SEC-09**: Clear actionable guidance in `install.sh` when checksum utilities are missing.
- **SEC-10**: Non-blocking `PRAGMA wal_checkpoint(PASSIVE)` sweep during scheduled maintenance GC.

#### Supported Platforms
- Linux: `amd64` (x86_64), `arm64` (aarch64)
- macOS (Darwin): `amd64` (Intel), `arm64` (Apple Silicon)
- Windows: `amd64` (x64), `arm64`

#### Known Limitations
- The HTTP API is strictly loopback-only by design; remote authentication is out of scope for v1.
- In-process SQLite requires local storage (NFS/network filesystems are not recommended for SQLite WAL mode).
