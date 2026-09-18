# Agentd

A self-hosted runner for scheduled checks over external sources.

When a monitored source changes shape and extraction breaks, Agentd detects the
breakage and proposes a verified repair. It never applies a repair without
human approval.

## Constraints

Single static binary. Go, no cgo, standard library wherever practical.
SQLite via `modernc.org/sqlite` in WAL mode. No external service dependency,
no required inbound network connectivity. BYOK for model providers. Plugins
speak MCP over stdio.

## Layout

Hexagonal, as a modular monolith. Dependencies point inward only:

```
driving adapters  ->  core  ->  ports  <-  driven adapters
```

| Path | Role |
| --- | --- |
| `cmd/agentd` | composition root: builds adapters, injects them into the core |
| `internal/core/...` | domain: scheduling, run orchestration, policy, repair orchestration |
| `internal/ports` | interfaces the core needs; declared by the core, implemented outside |
| `internal/api` | driving adapter: local HTTP surface |
| `internal/cli` | driving adapter: command line |
| `internal/store` | driven adapter: SQLite persistence |
| `internal/plugins` | driven adapter: MCP-over-stdio plugin host |
| `internal/adapters/...` | driven adapters: clock, HTTP fetch, model providers |
| `internal/config` | configuration loading |
| `migrations` | embedded SQL schema migrations |
| `fixtures` | recorded source payloads for deterministic tests |
| `tests` | cross-cutting tests, including the architecture boundary test |

The core performs no network I/O, no filesystem I/O, no database access, no
sleeping, no direct clock access and no model calls. Time, randomness and all
I/O arrive through `internal/ports`. `tests/architecture_test.go` enforces this
by walking the import graph.

## Status

Domain model, ports and scheduling are implemented and tested. No concrete
network, database or model adapters yet.
