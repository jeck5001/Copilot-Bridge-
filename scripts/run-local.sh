#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(dirname -- "$SCRIPT_DIR")
DATA_ROOT="$PROJECT_ROOT/data"
mkdir -p "$DATA_ROOT"
chmod 700 "$DATA_ROOT"

export M365_LISTEN="${M365_LISTEN:-127.0.0.1:4141}"
export M365_TOKEN_CACHE="${M365_TOKEN_CACHE:-$DATA_ROOT/accounts.json}"
export M365_SESSION_CACHE="${M365_SESSION_CACHE:-$DATA_ROOT/sessions.json}"
export M365_API_KEYS="${M365_API_KEYS:-$DATA_ROOT/api-keys.json}"
export M365_SETTINGS_FILE="${M365_SETTINGS_FILE:-$DATA_ROOT/settings.json}"
export M365_DEBUG_LOG="${M365_DEBUG_LOG:-$DATA_ROOT/debug-logs.jsonl}"
export M365_ADMIN_PASSWORD_HASH_FILE="${M365_ADMIN_PASSWORD_HASH_FILE:-$DATA_ROOT/admin-password.hash}"
export M365_ADMIN_PASSWORD="${M365_ADMIN_PASSWORD:-admin888}"
export M365_COOKIE_SECURE="${M365_COOKIE_SECURE:-false}"
export M365_LOG_LEVEL="${M365_LOG_LEVEL:-warn}"

cd "$PROJECT_ROOT"
if [ "$#" -gt 0 ]; then
    exec "$@"
fi
exec go run ./cmd/server
