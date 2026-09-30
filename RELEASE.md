# Agentd Release Engineering Guide

This document describes release procedures, cross-compilation tooling, signing, and integrity verification for Agentd releases.

---

## 1. Release Principles

1. **One Static Binary**: Every target binary is compiled with `CGO_ENABLED=0`. It requires no external C libraries, no system-level SQLite installation, and no runtime interpreter.
2. **Reproducible Builds**: All release builds utilize `-trimpath` to eliminate host-specific file paths and embed version metadata at link time.
3. **Semantic Versioning (SemVer)**: Releases follow `vMAJOR.MINOR.PATCH`:
   - Major: Breaking architectural changes or incompatible store migrations.
   - Minor: New features, commands, or adapter capabilities.
   - Patch: Bug fixes, security hardening, or documentation updates.

---

## 2. Release Targets & Binary Matrix

The cross-compilation tool ([`scripts/release/main.go`](scripts/release/main.go)) compiles and packages across 6 targets:

| Platform | Architecture | Archive Format | Archive Content |
| :--- | :--- | :--- | :--- |
| `linux` | `amd64` | `.tar.gz` | `agentd` (executable) |
| `linux` | `arm64` | `.tar.gz` | `agentd` (executable) |
| `darwin` | `amd64` | `.tar.gz` | `agentd` (executable) |
| `darwin` | `arm64` | `.tar.gz` | `agentd` (executable) |
| `windows` | `amd64` | `.zip` | `agentd.exe` (executable) |
| `windows` | `arm64` | `.zip` | `agentd.exe` (executable) |

---

## 3. Cryptographic Checksums, Signing, & SBOM

Every release generates:
1. **`SHA256SUMS`**: SHA-256 cryptographic digests for all compiled archives and raw executables.
2. **Cryptographic Signatures (`SHA256SUMS.sig`)**: Produced via [`scripts/sign.sh`](scripts/sign.sh) using GPG or Cosign keys configured in CI secrets.
3. **Software Bill of Materials (`agentd_<version>_sbom.json`)**: Generated using [`scripts/sbom/main.go`](scripts/sbom/main.go) in **CycloneDX 1.5 JSON format**, detailing all Go dependencies, module hashes, and toolchain versions extracted directly from the compiled binary.

---

## 4. Release Engineering Checklist

Before tagging and publishing a new release:

### 1. Verification Gates
Run the full test suite and verify 100% pass:
```bash
# Deterministic core branch coverage (>90%)
go test -v -count=1 ./internal/core/...

# Golden fixture corpus (50+ scenarios)
go test -v -count=1 ./tests -run "TestFixtureCorpus"

# Integration and E2E pipelines
go test -v -count=1 ./tests -run "TestPipelineIntegration|TestE2E"

# Security & Isolation
go test -v -count=1 ./tests -run "TestSecurity|TestMemoryScale"
```

### 2. Local Cross-Compilation Test
Verify cross-compilation builds cleanly for all 6 targets:
```bash
go run scripts/release/main.go
```

Verify that `dist/` contains all 6 archives, `SHA256SUMS`, and `agentd_<version>_sbom.json`.

### 3. Verify Install Script
```bash
go test -v ./tests -run "TestInstallScript"
```

### 4. Tag and Push
```bash
git tag -a v1.0.0 -m "Agentd Release v1.0.0"
git push origin v1.0.0
```

GitHub Actions will trigger [`.github/workflows/release.yml`](.github/workflows/release.yml), build the release matrix, sign checksums, and publish the release artifacts.
