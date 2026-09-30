# Agentd Troubleshooting Guide

This guide diagnoses common operational scenarios, error states, and health alerts using clear, user-facing explanations.

---

## 1. Understanding Error Classes

Agentd strictly separates structural extraction breakage from environmental network failures:

### Transient Network Outages (`ClassTransient`)
- **Symptoms**: HTTP 502/503/504 errors, connection refused, DNS lookup timeouts.
- **Agentd Behavior**: These are retried automatically within the current schedule slot using exponential backoff (up to `max_retries`, default: 3). They **never** open a repair incident or consume AI model tokens.
- **User Action**: Check remote service status or internet connectivity.

### Structural Extraction Breakage (`ClassStructural`)
- **Symptoms**: Source returns HTTP 200 OK, but the configured locator (CSS selector, JSONPath) yields empty text or fails to match any DOM node.
- **Agentd Behavior**: Agentd concludes that the source webpage redesigned or updated its DOM hierarchy. It halts execution, captures the snapshot, opens an incident, and evaluates a verified candidate binding.
- **User Action**: Inspect the incident with `agentd incident show <id>` and approve the proposed fix.

---

## 2. Healing & Incident FAQs

### Q: Why didn't Agentd automatically apply the repair?
**Answer**: By foundational architectural invariant, **repair is NEVER automatically applied**.

Automatic repair creates invisible drift, cascading errors, and silent extraction hallucinations. Instead, Agentd verifies candidate bindings against 5 multi-layer sanity gates (G1–G5), prepares a human-readable explanation, and waits for your explicit approval:
```bash
agentd repair approve <incident-id> --by "your-name"
```

### Q: What does "Global Circuit Breaker Triggered" mean?
- **Root Cause**: If more than 5 checks experience open incidents simultaneously across the system, Agentd trips its global circuit breaker.
- **Why This Exists**: Prevents burning your AI model token budget during widespread outages (e.g. an entire upstream domain changing its authentication wall or redesigning simultaneously).
- **Resolution**:
  1. Inspect open incidents: `agentd incident list`
  2. Resolve or reject invalid incidents: `agentd repair reject <incident-id> --by "operator"`
  3. Once open incidents drop to 5 or fewer, the circuit breaker resets automatically.

### Q: What does "Repair budget exhausted" mean?
- **Root Cause**: Agentd generated up to 3 candidate repair bindings, but none passed all 5 verification gates (e.g. they failed schema compatibility or extracted blank text).
- **User Action**: The check needs human operator attention. Update the check locator manually using `agentd check add` or update the definition.

---

## 3. Freshness & Staleness Alerts

Observation freshness is Agentd's **primary Service Level Indicator (SLI)**.

### Inspecting Staleness
```bash
agentd status
```
Output:
```text
Checks: 12 active (12 total)
Freshness: 11 fresh, 1 overdue
Max Staleness: 142.0s
```

### Why is a check marked overdue?
1. **Worker Pool Saturation**: The concurrency limit was reached and the check was queued.
2. **Backpressure**: The dispatch queue overflowed, causing the run to be recorded as `skipped_overload`.
3. **Daemon Downtime**: Agentd was stopped. Upon restart, the configured `catch_up` policy (`once`, `skip`, or `backfill`) brings the check back to freshness.

---

## 4. Database & Filesystem Health

### Error: "refusing to open database file with insecure permissions"
- **Cause**: On POSIX systems, Agentd enforces that `agentd.db` has mode `0600` (`-rw-------`). If the file is world-readable (`0004` or `0002`), it refuses to start.
- **Fix**:
  ```bash
  chmod 0600 /path/to/agentd.db
  ```

### Database Size & WAL Checkpoints
SQLite runs in WAL mode (`journal_mode = WAL`) with a single-writer connection pool. Readers never block writers, and writers never block readers.

If the WAL file (`agentd.db-wal`) grows excessively:
```bash
# Perform manual retention sweep and checkpoint:
agentd gc
```

---

## 5. Crash Recovery

If the host machine power cuts or Agentd is terminated abruptly (`SIGKILL` / `kill -9`):

At startup, Agentd scans for non-terminal runs (`pending`, `running`):
- Non-terminal runs are transitioned to `interrupted`.
- All partial transactions roll back cleanly via SQLite's ACID write-ahead log.
- Schedulers resume normal slot dispatch immediately without data corruption.
