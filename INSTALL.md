# Installing Agentd

Agentd is distributed as a single static binary with no runtime dependencies. There is no external database to configure, no C runtime required (`CGO_ENABLED=0`), and no dynamic libraries to install.

---

## Supported Platforms

| Operating System | Architecture | Package Format | Binary Name |
| :--- | :--- | :--- | :--- |
| **Linux** | `amd64` (x86_64) | `.tar.gz` | `agentd` |
| **Linux** | `arm64` (aarch64) | `.tar.gz` | `agentd` |
| **macOS** (Darwin) | `amd64` (Intel) | `.tar.gz` | `agentd` |
| **macOS** (Darwin) | `arm64` (Apple Silicon) | `.tar.gz` | `agentd` |
| **Windows** | `amd64` (x64) | `.zip` | `agentd.exe` |
| **Windows** | `arm64` | `.zip` | `agentd.exe` |

---

## Method 1: Automated Audited Installer (Recommended for Linux/macOS)

The official installer detects your OS and architecture, downloads the release tarball, strictly verifies the SHA-256 checksum against `SHA256SUMS`, and installs `agentd` into `/usr/local/bin` (or `$HOME/.local/bin`):

```bash
curl -sSfL https://raw.githubusercontent.com/champion19007/agentd/main/install.sh | sh
```

### Custom Installation Directory
You can customize the target installation directory using `BINDIR`:
```bash
curl -sSfL https://raw.githubusercontent.com/champion19007/agentd/main/install.sh | BINDIR=$HOME/bin sh
```

---

## Method 2: Manual Download & Verification

For air-gapped environments or production servers:

### 1. Download Artifacts
Download the archive and `SHA256SUMS` from the [GitHub Releases page](https://github.com/champion19007/agentd/releases):
```bash
VERSION="v1.0.0"
OS="linux"    # or darwin, windows
ARCH="amd64"  # or arm64

curl -LO "https://github.com/champion19007/agentd/releases/download/${VERSION}/agentd_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -LO "https://github.com/champion19007/agentd/releases/download/${VERSION}/SHA256SUMS"
```

### 2. Verify Cryptographic Checksum
Always verify the archive integrity prior to extraction:
```bash
# On Linux:
sha256sum --check --ignore-missing SHA256SUMS

# On macOS:
shasum -a 256 --check --ignore-missing SHA256SUMS
```

If the checksum does not match, abort immediately.

### 3. Extract and Move to PATH
```bash
tar -xzf "agentd_${VERSION}_${OS}_${ARCH}.tar.gz"
sudo mv agentd /usr/local/bin/
sudo chmod 0755 /usr/local/bin/agentd
```

---

## Method 3: Building from Source

Prerequisites: Go 1.22 or later.

```bash
git clone https://github.com/champion19007/agentd.git
cd agentd

# Build static binary with no cgo
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o agentd ./cmd/agentd

# Verify binary
./agentd version
```

---

## Running Agentd as a Systemd Service

For persistent daemon operation on Linux servers:

### 1. Create Dedicated Service User
```bash
sudo useradd -r -s /bin/false -m -d /var/lib/agentd agentd
```

### 2. Initialize the Database
```bash
sudo -u agentd agentd init --db /var/lib/agentd/agentd.db
```

### 3. Install Systemd Unit File (`/etc/systemd/system/agentd.service`)
```ini
[Unit]
Description=Agentd Monitoring Daemon
After=network.target

[Service]
Type=simple
User=agentd
Group=agentd
WorkingDirectory=/var/lib/agentd
ExecStart=/usr/local/bin/agentd serve --addr 127.0.0.1:8080 --db /var/lib/agentd/agentd.db
Restart=always
RestartSec=5s

# Security Hardening
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/agentd
PrivateTmp=true
NoNewPrivileges=true
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

### 4. Enable and Start Service
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now agentd
sudo systemctl status agentd
```

---

## Running with Docker

Agentd packages cleanly into a scratch or Alpine container:

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY agentd /usr/local/bin/agentd
VOLUME ["/data"]
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agentd"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--db", "/data/agentd.db", "--allow-remote"]
```

---

## Uninstalling Agentd

To completely remove Agentd from your system:

### 1. Stop and Disable Services (if running)
```bash
sudo systemctl disable --now agentd 2>/dev/null || true
sudo rm -f /etc/systemd/system/agentd.service
sudo systemctl daemon-reload
```

### 2. Remove Binary
```bash
# System-wide installation
sudo rm -f /usr/local/bin/agentd

# User-local installation
rm -f "$HOME/.local/bin/agentd" "$HOME/bin/agentd"
```

### 3. Remove Data & Configuration (Optional)
If you wish to remove all databases, snapshots, and audit logs:
```bash
rm -rf /var/lib/agentd
rm -f "$HOME/.agentd.db"*
```
