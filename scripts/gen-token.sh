#!/usr/bin/env bash
# gen-token.sh - Headless token generator alias
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$DIR/gen-freebuff-token.sh" "$@"
