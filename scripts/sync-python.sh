#!/usr/bin/env bash
# Copies the Python recorder from the private Agent Pulse build into python/.
#
# Only the files the private build tracks are copied, so a local virtual environment, a cache or a scratch file there can never be published here. What is removed there is removed here too. Run it after a change to the Python recorder is merged, and commit what it writes.
set -euo pipefail

AP=${AP_BUILD:-"$HOME/alis.build/techbridge/build/ap"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

if ! git -C "$AP" rev-parse --git-dir >/dev/null 2>&1; then
  echo "no Agent Pulse build at $AP; set AP_BUILD to where it is" >&2
  exit 1
fi

rm -rf "$ROOT/python"
git -C "$AP" ls-files -z -- recorder/python | while IFS= read -r -d '' file; do
  target="$ROOT/python/${file#recorder/python/}"
  mkdir -p "$(dirname "$target")"
  cp "$AP/$file" "$target"
done
echo "copied $(git -C "$AP" ls-files -- recorder/python | wc -l) files from $(git -C "$AP" rev-parse --short HEAD)"
