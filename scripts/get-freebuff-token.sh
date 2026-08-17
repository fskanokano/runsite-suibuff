#!/usr/bin/env bash
# get-freebuff-token.sh — legacy alias for gen-freebuff-token.sh
# Generates a FreeBuff auth token via zero-dependency headless OAuth login.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN_SCRIPT="$SCRIPT_DIR/gen-freebuff-token.sh"
if [ ! -f "$GEN_SCRIPT" ] && [ -f "$SCRIPT_DIR/gen-token.sh" ]; then
  GEN_SCRIPT="$SCRIPT_DIR/gen-token.sh"
fi

exec "$GEN_SCRIPT" "$@"
