# Agentd Security Model & Hardening Guide

Agentd runs in security-sensitive environments, fetching data from untrusted public web pages and APIs while processing sensitive credentials and executing AI models. This document describes the threat model and defensive hardening implemented in Agentd.

---

## 1. Network Hardening & SSRF Prevention

Monitored URLs are untrusted input. The HTTP source adapter ([`internal/adapters/httpsource`](internal/adapters/httpsource/httpsource.go)) enforces strict Server-Side Request Forgery (SSRF) protections:

### Blocked Destinations
- **Private RFC 1918 Networks**: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`.
- **Loopback Addresses**: `127.0.0.0/8`, `::1` (blocked by default on fetches; allowed only when testing or explicitly configured).
- **Link-Local & Cloud Metadata Endpoints**: `169.254.0.0/16` and `fe80::/10`. Requests to AWS/GCP/Azure instance metadata endpoints (`http://169.254.169.254/`) are intercepted and rejected before TCP dial.
- **Broadcast & Carrier-Grade NAT**: `255.255.255.255`, `100.64.0.0/10`.

### DNS Rebinding Mitigation
The HTTP transport resolves the domain prior to establishing the TCP connection and re-validates the resulting IP against the blocklist during the `DialContext` hook. If a hostname resolves to a forbidden IP address, the connection terminates before any HTTP request bytes are sent.

### Response Bounds
- Maximum HTTP response body: **5 MB** (excess streams are truncated with an error).
- Maximum extracted payload: **100 KB**.
- Slowloris and connection timeouts: 10s dial timeout, 30s response header timeout.

---

## 2. Filesystem & Database Protection

- **File Permissions**: The SQLite database file and WAL logs are created with strict `0600` permissions (`-rw-------`).
- **World-Readable Refusal**: At startup, Agentd inspects the database file mode. If the file is world-readable (`0004` or `0002` set on POSIX platforms), Agentd refuses to open it with a fatal security error.
- **Directory Traversal Defense**: All path inputs (`agentd backup --to <path>`, `agentd check export --out <path>`) are evaluated with `filepath.Clean` and validated against allowed destination boundaries.

---

## 3. Model Privacy & Prompt Injection Fencing

- **Zero-Cost Hash Gating**: If a fetched source payload matches the SHA-256 digest of the previous run, Agentd halts execution immediately. No text is sent to the LLM provider, ensuring complete privacy and zero token consumption.
- **Fenced Prompts**: Source payloads are fed into LLMs as passive, quoted data blocks within structural delimiters (`<untrusted_source_payload>`). System instructions instruct the model to treat all payload text as passive string data, preventing prompt injection instructions from overriding extraction tasks.
- **Strict JSON Output Schemas**: All model responses are validated against strongly typed Go structs. Hallucinations or malformed fields fail verification gates (G1–G5).

---

## 4. Subprocess Sandboxing & Secret Hygiene

Plugins communicate via Model Context Protocol (MCP) as child processes over `stdio`. The plugin host ([`internal/plugins`](internal/plugins/mcp.go)) enforces strict isolation:

- **Isolated Working Directories**: Each plugin runs in a private sandbox directory (`0700`) with no access to sibling plugin directories.
- **Zero Inherited Environment**: The plugin subprocess does NOT inherit the daemon's environment variables. Secrets such as `GEMINI_API_KEY`, database paths, or cloud tokens are completely invisible to plugins.
- **Explicit Scoped Secrets**: Only credentials explicitly bound via `SecretRef` are resolved and passed to the specific plugin.
- **Stderr Scrubbing**: Stderr logs and error strings are filtered through a pattern scrubber before being recorded or displayed, preventing accidental credential leakage.

---

## 5. Tamper-Evident Audit Log

Agentd maintains an append-only audit trail in SQLite. Every operator action (`check add`, `repair approve`, `repair reject`, `gc`, `backup`) is recorded as an immutable `AuditEvent`.

Each event includes:
- Timestamp (RFC 3339 UTC)
- Actor (`sre-operator`, `system`)
- Action taken
- Cryptographic SHA-256 hash chaining of previous event digests

Any manual tampering with the audit database breaks the hash chain verification.

---

## 6. Vulnerability Disclosure

If you discover a security issue in Agentd, please report it via private email to `security@agentd.io` or open a GitHub Security Advisory. Do not file public issues for zero-day vulnerabilities.
