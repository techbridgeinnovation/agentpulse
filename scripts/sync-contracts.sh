#!/usr/bin/env bash
# Copies the generated Go for the metering and governance contracts into recorder/pb.
#
# The source is the private Agent Pulse build's module cache, so this runs on a machine that can resolve alis.build modules. Everyone else consumes the copies checked in here. Run it after a contract release, then commit recorder/pb and bump the recorder's tag.
set -euo pipefail

AP=${AP_RECORDER:-"$HOME/alis.build/techbridge/build/ap/recorder/v1"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

for contract in metering governance; do
  module="alis.build/techbridge/ap/$contract"
  dir=$(cd "$AP" && go list -m -f '{{.Dir}}' "$module")
  version=$(cd "$AP" && go list -m -f '{{.Version}}' "$module")
  target="$ROOT/recorder/pb/$contract"
  rm -f "$target"/*.go
  cp "$dir"/*.go "$target/"
  chmod u+w "$target"/*.go
  echo "$contract $version"
done | tee "$ROOT/recorder/pb/VERSIONS"

cd "$ROOT/recorder" && go build ./... && (cd adkv2hooks && go build ./...)
