#!/usr/bin/env bash
# Copies the documentation pages from the private Agent Pulse build into docs/.
#
# The pages are markdown the console compiles to components at build time, and the same files are what a team without an account reads here. One source, so what the console shows and what this repository publishes cannot drift. Run it after changing a page, and commit what it writes.
set -euo pipefail

AP=${AP_DOCS:-"$HOME/alis.build/techbridge/build/ap/portal/v1/app/docs"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

if [ ! -d "$AP" ]; then
  echo "no docs at $AP; set AP_DOCS to where they are" >&2
  exit 1
fi

rm -f "$ROOT/docs"/*.md
for page in "$AP"/*.md; do
  cp "$page" "$ROOT/docs/"
  echo "$(basename "$page")"
done
