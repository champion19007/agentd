#!/bin/sh
# Agentd Installer
# Downloads, verifies SHA-256 checksum, and installs the agentd static binary.
set -eu

REPO="champion19007/agentd"
VERSION="${AGENTD_VERSION:-v1.0.0}"
BINDIR="${BINDIR:-}"

# 1. Detect OS and Architecture
OS="${AGENTD_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
ARCH="${AGENTD_ARCH:-$(uname -m)}"

case "$OS" in
    linux*)   OS="linux" ;;
    darwin*)  OS="darwin" ;;
    mingw*|msys*|cygwin*|windows*)
        echo "Notice: Windows environment detected. Proceeding with Linux binary target..." >&2
        OS="linux"
        ;;
    *) echo "Error: Unsupported OS '$OS'. Agentd supports Linux and macOS." >&2; exit 1 ;;
esac

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Error: Unsupported architecture '$ARCH'. Agentd supports amd64 and arm64." >&2; exit 1 ;;
esac

# 2. Determine installation directory
if [ -z "$BINDIR" ]; then
    if [ -w "/usr/local/bin" ]; then
        BINDIR="/usr/local/bin"
    elif [ "$(id -u 2>/dev/null || echo 1000)" -eq 0 ]; then
        BINDIR="/usr/local/bin"
    else
        BINDIR="${HOME:-.}/.local/bin"
        mkdir -p "$BINDIR"
    fi
fi

# 3. Create secure temporary workspace
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'agentd-install')"
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

ARCHIVE="agentd_${VERSION}_${OS}_${ARCH}.tar.gz"
URL_BASE="https://github.com/${REPO}/releases/download/${VERSION}"

echo "==> Installing Agentd ${VERSION} (${OS}/${ARCH})..."
echo "==> Downloading release artifacts..."

# In local testing or fallback, allow AGENTD_DIST_URL or local file override
if [ -n "${AGENTD_DIST_URL:-}" ]; then
    URL_BASE="$AGENTD_DIST_URL"
fi

if command -v curl >/dev/null 2>&1; then
    curl -sSfL "${URL_BASE}/${ARCHIVE}" -o "${TMP_DIR}/${ARCHIVE}"
    curl -sSfL "${URL_BASE}/SHA256SUMS" -o "${TMP_DIR}/SHA256SUMS"
elif command -v wget >/dev/null 2>&1; then
    wget -qO "${TMP_DIR}/${ARCHIVE}" "${URL_BASE}/${ARCHIVE}"
    wget -qO "${TMP_DIR}/SHA256SUMS" "${URL_BASE}/SHA256SUMS"
else
    echo "Error: curl or wget is required to install Agentd." >&2
    exit 1
fi

# 4. Strict Checksum Verification
echo "==> Verifying SHA-256 checksum..."
cd "$TMP_DIR"

EXPECTED_HASH="$(grep -F "${ARCHIVE}" SHA256SUMS | awk '{print $1}')"
if [ -z "$EXPECTED_HASH" ]; then
    echo "Error: Release checksum for ${ARCHIVE} not found in SHA256SUMS." >&2
    exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_HASH="$(sha256sum "${ARCHIVE}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL_HASH="$(shasum -a 256 "${ARCHIVE}" | awk '{print $1}')"
else
    echo "Error: Neither 'sha256sum' nor 'shasum' was found in your PATH." >&2
    echo "Agentd requires cryptographic checksum verification before executing downloaded binaries." >&2
    echo "Please install coreutils, perl-Digest-SHA, or busybox with sha256sum support to proceed." >&2
    exit 1
fi

if [ "$EXPECTED_HASH" != "$ACTUAL_HASH" ]; then
    echo "SECURITY ERROR: Checksum verification failed!" >&2
    echo "  Expected: $EXPECTED_HASH" >&2
    echo "  Actual:   $ACTUAL_HASH" >&2
    echo "Aborting installation to prevent executing compromised binaries." >&2
    exit 1
fi
echo "==> Checksum verified successfully."

# 5. Extract and Install
echo "==> Extracting binary..."
tar -xzf "${ARCHIVE}"
chmod 0755 agentd

echo "==> Installing agentd to ${BINDIR}/agentd..."
if [ -w "$BINDIR" ]; then
    mv agentd "${BINDIR}/agentd"
else
    sudo mv agentd "${BINDIR}/agentd"
fi

# 6. Verify Installation
if command -v agentd >/dev/null 2>&1 || [ -x "${BINDIR}/agentd" ]; then
    echo "==> Success! Agentd is installed:"
    "${BINDIR}/agentd" version
else
    echo "==> Installed to ${BINDIR}/agentd. Ensure ${BINDIR} is in your PATH."
fi
