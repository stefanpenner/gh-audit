#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
CONFIG="$SCRIPT_DIR/open-source.yaml"
DB="$SCRIPT_DIR/audit.db"

if [ -z "${GITHUB_TOKEN:-}" ]; then
  echo "error: GITHUB_TOKEN is not set" >&2
  echo "Create a PAT at https://github.com/settings/tokens with public_repo scope" >&2
  exit 1
fi

cd "$ROOT_DIR"
go build -o gh-audit .

echo "==> Syncing nodejs/node and rails/rails..."
./gh-audit sync --config "$CONFIG" --db "$DB" --verbose

echo "==> Generating report..."
./gh-audit report --config "$CONFIG" --db "$DB"

echo "Done. Database at: $DB"
