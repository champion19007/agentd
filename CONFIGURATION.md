# Agentd Configuration Reference

Agentd operates with explicit defaults and single-source configuration. All runtime settings can be managed via command-line flags, environment variables, or declarative Job Schemas.

---

## 1. Global CLI Flags

Every command supports these flags:

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-db <path>` | `agentd.db` | Path to the SQLite database file. Created automatically with `0600` permissions. |
| `--json` | `false` | Emits all responses and errors as structured JSON for automation. |

---

## 2. Environment Variables

| Variable | Description |
| :--- | :--- |
| `AGENTD_DB_PATH` | Default database file path when `-db` is not specified on the CLI. |
| `GEMINI_API_KEY` | Google Gemini API key for change evaluation and repair proposals. |
| `OPENAI_API_KEY` | OpenAI API key for alternative BYOK model execution. |
| `ANTHROPIC_API_KEY` | Anthropic Claude API key for alternative BYOK model execution. |
| `AGENTD_SIGNING_KEY` | GPG key ID used by release signing hooks. |
| `COSIGN_KEY` | Cosign private key file or KMS URI for artifact signing. |

---

## 3. Job Schema Specification

Checks can be configured either via the CLI (`agentd check add`) or declaratively in JSON files (`agentd check add --file check.json` / `agentd check export`).

### Canonical Job Schema (Current: v2)

```json
{
  "schema_version": 2,
  "id": "pricing-tier",
  "name": "Enterprise Plan Pricing",
  "source": {
    "kind": "http",
    "url": "https://example.com/pricing",
    "method": "GET",
    "headers": {
      "Accept-Language": "en-US"
    }
  },
  "schedule": {
    "interval": "15m",
    "jitter": 0.05,
    "catch_up": "once"
  },
  "intent": {
    "kind": "scalar",
    "name": "Enterprise Plan Pricing"
  },
  "binding": {
    "dialect": "css",
    "target": "value",
    "expression": ".plan-enterprise .price"
  },
  "destination": {
    "kind": "notify",
    "target": "https://hooks.slack.com/services/T00/B00/X00",
    "on_quiet": false
  },
  "policy": {
    "priority": 10,
    "max_repair_attempts": 3,
    "retain_snapshots": 5,
    "max_retries": 3,
    "retry_backoff": "30s"
  }
}
```

### Supported Previous Job Schema (Legacy: v1)

Agentd automatically migrates v1 schemas forward to v2 upon load:

```json
{
  "version": 1,
  "id": "pricing-tier",
  "name": "Enterprise Plan Pricing",
  "url": "https://example.com/pricing",
  "interval": "15m",
  "selector": ".plan-enterprise .price",
  "target": "value",
  "notify_url": "https://hooks.slack.com/services/T00/B00/X00",
  "retries": 3
}
```

> [!IMPORTANT]
> **Forward-Only Schema Migration:**  
> Loading an older schema (v1) automatically forward-migrates it in memory to v2. If the check or job definition is subsequently edited, updated, or saved back to disk by user action, CLI command, or API mutation, it will be written back exclusively using the current v2 format. Backward migration from v2 to v1 is intentionally unsupported.

---

## 4. Scheduling & Catch-Up Policies

The scheduler operates on deterministic slot indices (`Slot = instant / interval`), making duplicate execution mathematically impossible.

| Policy | Behavior | Use Case |
| :--- | :--- | :--- |
| `once` *(default)* | Schedules exactly one run covering missed daemon downtime. | Ideal for web scraping and rate-limited endpoints. |
| `skip` | Ignores all missed slots and only schedules the current instant. | Ideal for high-frequency volatile sensors. |
| `backfill` | Chronologically backfills all missed slots since last execution. | Ideal for auditable log streams and metric feeds. |

---

## 5. Retention & Garbage Collection

Retention rules enforce bounded disk growth while preserving critical audit evidence:

- **Most Recent Known-Good Snapshot**: Permanently protected from deletion.
- **Intermediate Snapshots**: Bounded by `retain_snapshots` (default: 5 per check).
- **Run History**: Retained for 30 days by default.
- **Incident History**: Retained for 90 days.
- **Audit Log**: Immutable append-only hash chain; never pruned.

Run manual garbage collection:
```bash
# Preview what would be pruned:
agentd gc --dry-run

# Execute retention sweep:
agentd gc --runs 30d --snapshots 5 --incidents 90d
```

---

## 6. 4-Tier Graceful Degradation Hierarchy

Under severe resource constraints, Agentd sheds non-essential tasks in strict hierarchical order:

1. **Tier 1 (`shed_healing`)**: Pauses LLM repair generation. Broken checks open an incident but do not spend model tokens.
2. **Tier 2 (`shed_model`)**: Bypasses LLM semantic change evaluation. Unchanged checks complete via hash gates; changed checks record a fallback change event without LLM summaries.
3. **Tier 3 (`reduced_retention`)**: Captures only the raw payload without storing intermediate transformation buffers.
4. **Tier 4 (`shed_runs`)**: Physical queue overflow returns `skipped_overload`. Observation is the last capability to be sacrificed.
