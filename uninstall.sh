#!/usr/bin/env bash
set -euo pipefail

BIN_PATH="$HOME/.local/bin/agy-gzip-proxy"
PLIST_LABEL="io.github.agy-gzip-proxy"
PLIST_PATH="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
UID_NUM="$(id -u)"

launchctl bootout "gui/${UID_NUM}/${PLIST_LABEL}" >/dev/null 2>&1 || true
rm -f "$PLIST_PATH" "$BIN_PATH"

echo "Unloaded ${PLIST_LABEL} and removed binary/plist."
echo "If you added an agy() wrapper to ~/.zshrc, remember to remove it manually."
