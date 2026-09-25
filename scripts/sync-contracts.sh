#!/usr/bin/env bash
# Copies the generated Go for the parts of the metering and governance contracts the recorder speaks into recorder/pb.
#
# The source is the private Agent Pulse build's module cache, so this runs on a machine that can resolve alis.build modules. Everyone else consumes the copies checked in here. Run it after a contract release, then commit recorder/pb and bump the recorder's tag.
#
# Only the files the recorder compiles against are copied. A generated file carries the whole of its proto file — every message and every method — so copying a package whole would publish calls the gateway never forwards to a recorder. What is here is what a recorder can reach.
set -euo pipefail

AP=${AP_RECORDER:-"$HOME/alis.build/techbridge/build/ap/recorder/v1"}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

# proto file stems per contract; each stem yields <stem>.pb.go, <stem>_grpc.pb.go and <stem>_grpc.meta.pb.go.
declare -A FILES=(
  [metering]="activity priceable_unit user"
  [governance]="decision"
)

# proto file stems a copied file imports for its types alone: only <stem>.pb.go is copied, so its messages compile and none of its calls can be made.
declare -A TYPES_ONLY=(
  [metering]="agent"
  [governance]=""
)

for contract in metering governance; do
  module="alis.build/techbridge/ap/$contract"
  dir=$(cd "$AP" && go list -m -f '{{.Dir}}' "$module")
  version=$(cd "$AP" && go list -m -f '{{.Version}}' "$module")
  target="$ROOT/recorder/pb/$contract"
  rm -f "$target"/*.go
  for stem in ${FILES[$contract]}; do
    # Named one by one rather than globbed: the message file and the two grpc stubs share a stem but not a separator, and a glob on the stem alone would also take any other file that happens to start the same way.
    cp "$dir/$stem.pb.go" "$dir/${stem}_grpc.pb.go" "$dir/${stem}_grpc.meta.pb.go" "$target/"
  done
  for stem in ${TYPES_ONLY[$contract]}; do
    cp "$dir/$stem.pb.go" "$target/"
  done
  chmod u+w "$target"/*.go
  echo "$contract $version"
done | tee "$ROOT/recorder/pb/VERSIONS"

cd "$ROOT/recorder" && go build ./... && (cd adkv2hooks && go build ./...)
