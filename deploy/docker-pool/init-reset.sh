#!/usr/bin/env sh
set -eu

# Convenience wrapper:
# reset data and redeploy with the selected pool size.
#
# Examples:
#   ./init-reset.sh -3
#   ./init-reset.sh --instances 2

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
cd "$SCRIPT_DIR"

exec ./deploy.sh --init "$@"
