#!/bin/sh
# Signing hook for Agentd release artifacts.
#
# Usage:
#   ./scripts/sign.sh <file-to-sign>
#
# Supported signers:
#   1. Cosign (if COSIGN_KEY or COSIGN_EXPERIMENTAL is set)
#   2. GPG (if AGENTD_GPG_KEY or default GPG keyring exists)

set -e

FILE="$1"
if [ -z "$FILE" ]; then
    echo "Usage: $0 <file-to-sign>" >&2
    exit 1
fi

if [ ! -f "$FILE" ]; then
    echo "Error: file not found: $FILE" >&2
    exit 1
fi

SIG_FILE="${FILE}.sig"

if [ -n "$COSIGN_KEY" ] && command -v cosign >/dev/null 2>&1; then
    echo "Signing $FILE with cosign..."
    cosign sign-blob --key "$COSIGN_KEY" --output-signature "$SIG_FILE" "$FILE"
    echo "Generated signature at $SIG_FILE"
    exit 0
fi

if command -v gpg >/dev/null 2>&1; then
    if [ -n "$AGENTD_GPG_KEY" ]; then
        echo "Signing $FILE with GPG key $AGENTD_GPG_KEY..."
        gpg --batch --yes --detach-sign --armor --default-key "$AGENTD_GPG_KEY" --output "$SIG_FILE" "$FILE"
        echo "Generated signature at $SIG_FILE"
        exit 0
    elif gpg --list-secret-keys >/dev/null 2>&1; then
        echo "Signing $FILE with default GPG secret key..."
        gpg --batch --yes --detach-sign --armor --output "$SIG_FILE" "$FILE" 2>/dev/null || true
        if [ -f "$SIG_FILE" ]; then
            echo "Generated signature at $SIG_FILE"
            exit 0
        fi
    fi
fi

echo "Notice: No signing key configured (AGENTD_GPG_KEY or COSIGN_KEY). Skipping release signing."
exit 0
