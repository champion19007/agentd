# Agentd

> **A single static binary runner for scheduled checks over external sources.**

When a monitored website or API changes shape and data extraction breaks, Agentd detects the structural failure, classifies the root cause, and generates a verified repair proposal.

**Fundamental Rule: Repair is NEVER automatically applied.** Agentd verifies candidate bindings against multi-layer sanity gates, presents a human-readable explanation, and waits for explicit operator approval before activating any new binding.

---

## 10-Minute Quickstart

Follow these 6 steps to install, monitor, understand, and repair your first check.

### Step 1: Install Agentd (30 seconds)

Agentd is distributed as a single static binary with no runtime dependencies.

```bash
# Install via the audited installer (verifies SHA-256 checksum):
curl -sSfL https://raw.githubusercontent.com/champion19007/agentd/main/install.sh | sh

# Or initialize a local workspace:
agentd init --db agentd.db
```

Verify your installation:
```bash
agentd version
```

---

### Step 2: Create Your First Check (1 minute)

Monitor a pricing page or status page every 15 minutes:

```bash
agentd check add \
  --name "Product Pricing" \
  --url "https://example.com/pricing" \
  --interval 15m \
  --selector ".plan-price"
```

Agentd creates the check and initializes an inferred CSS locator binding.

---

### Step 3: Run the Check (30 seconds)

Trigger an immediate execution without waiting for the next scheduler slot:

```bash
agentd check run Product-Pricing
```

View the execution run history:
```bash
agentd run list
```

---

### Step 4: Understand the Result (2 minutes)

Inspect the latest run details:

```bash
agentd run show --id <run-id>
```

#### The Architecture Principle: Calm Silence
- **Unchanged Sources**: When the source payload has not changed, Agentd's **zero-cost hash gate** halts execution immediately. No LLM calls are made, no tokens are spent, and no noisy alerts are emitted.
- **Terminal States**:
  - `quiet`: The value was extracted and matches the previous baseline.
  - `changed`: The value legitimately changed (e.g. price increased from \$20 to \$25).
  - `structural_break`: The CSS selector failed to resolve because the source HTML redesigned.
  - `skipped_overload`: The system saturated its queue and shed execution safely.

---

### Step 5: Configure Notifications (2 minutes)

Route state changes and breakage alerts to a webhook or Slack channel:

```bash
agentd check add \
  --name "Production Status" \
  --url "https://status.example.com" \
  --interval 5m \
  --selector "#system-status" \
  --notify notify \
  --dest-target "https://hooks.slack.com/services/YOUR/WEBHOOK/URL"
```

Notifications include run-level trace IDs and user-friendly explanations:
> *"Agentd could not find the pricing table on https://example.com/pricing. The source structure appears to have changed."*

---

### Step 6: Inspect and Approve a Repair (3 minutes)

When an extraction breaks, Agentd automatically opens an incident and verifies a new candidate binding against 5 verification gates (G1–G5).

#### 1. List Open Incidents
```bash
agentd incident list
```

#### 2. Inspect the Repair Proposal
```bash
agentd incident show --id <incident-id>
```

Agentd explains what broke and what it proposes:
```text
Incident: inc-9f8a2b (Check: Product-Pricing)
Status:   awaiting_approval
Breakage: Selector '.plan-price' returned nil (source markup changed from <div> to <section>)
Proposed: '.pricing-card .val' (verified against known-good schema, non-empty text extracted)
Budget:   Attempt 1 of 3
```

#### 3. Human Approval (Mandatory)
Activate the verified repair:
```bash
agentd repair approve --id <incident-id> --by "sre-operator"
```

The new binding is instantly activated and recorded in the immutable audit log!

---

## Feature Overview

| Capability | Guarantee |
| :--- | :--- |
| **Pure Static Binary** | Single Go binary with `CGO_ENABLED=0`. Zero external C dependencies. |
| **Persistence Engine** | SQLite WAL mode (`modernc.org/sqlite`) with single-writer serialization, zstd compression, and hot backups. |
| **Deterministic Core** | Pure business logic with zero wall-clock dependencies and 100% branch coverage (>90%). |
| **Conservative Healing** | Multi-gate verification (G1–G5), bounded budgets (max 3), global circuit breaker (>5 incidents). |
| **Observation Freshness** | Primary SLI tracking whether checks produce terminal runs within expected cadence. |
| **Security Hardened** | SSRF prevention (private IP / cloud metadata blocking), 0600 file modes, secret scrubbing. |
| **Graceful Degradation** | 4-tier load shedding: Healing -> Model -> Reduced Retention -> Scheduled Runs. |
| **Local API & MCP** | Loopback-bound REST API (`/v1/...`) and stdio Model Context Protocol (MCP) server. |

---

## Documentation Guides

- [Installation & Service Setup (INSTALL.md)](INSTALL.md)
- [Configuration Reference & BYOK (CONFIGURATION.md)](CONFIGURATION.md)
- [Security Hardening & Threat Model (SECURITY.md)](SECURITY.md)
- [MCP Plugin Development Guide (PLUGIN_DEVELOPMENT.md)](PLUGIN_DEVELOPMENT.md)
- [Operational Troubleshooting (TROUBLESHOOTING.md)](TROUBLESHOOTING.md)
- [Architecture & Design Invariants (ARCHITECTURE.md)](ARCHITECTURE.md)
- [Schema & Sequential Migrations (MIGRATIONS.md)](MIGRATIONS.md)
- [Release Engineering & SBOM (RELEASE.md)](RELEASE.md)

---

## License

Licensed under the Apache License, Version 2.0.
