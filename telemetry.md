# Agentd Telemetry Policy

## Default Setting: Strictly Disabled

By default, Agentd collects **zero** telemetry. No data, events, or metrics are transmitted outside your local machine.

## Opt-In Behavior

Telemetry is strictly opt-in. To enable local structural telemetry logging, the operator must explicitly set the environment variable:

```bash
AGENTD_TELEMETRY=1
```

If this variable is not set to `1`, telemetry collection is inactive and incurs zero network or storage overhead.

## Privacy and Data Invariants

When opted into, Agentd enforces the following privacy principles by architecture:

1. **No URLs or Hostnames**: Target endpoints, web addresses, or internal network hostnames are NEVER recorded.
2. **No Payload Data**: Monitored web page content, extracted fields, DOM text, or HTTP response bodies are NEVER recorded.
3. **No Identifiers or Credentials**: Check names, API keys, bearer tokens, passwords, cookies, authorization headers, and incident identifiers are strictly excluded.
4. **Structural Hashes Only**: When recording change events, only the 64-character SHA-256 hash of the extracted structure is captured to track drift patterns without retaining content.
5. **High-Level Outcomes Only**: Telemetry records coarse categories (e.g. `run`, `repair`) and state outcomes (e.g. `changed`, `quiet`, `failed`, `healed`), along with coarse durations in milliseconds.

## Example Telemetry Event Format

```json
{
  "schema_version": 1,
  "timestamp": "2026-09-29T10:00:00Z",
  "category": "run",
  "outcome": "changed",
  "duration_ms": 142,
  "structural_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}
```

## Security Audits

All telemetry code is covered by automated regression tests in `internal/telemetry/telemetry_test.go` ensuring that sensitive parameters and raw strings cannot leak into telemetry streams.
