#!/usr/bin/env bash
# Writes the Cursor rule from the adoption skill, so Claude Code and Cursor follow the same steps.
#
# The skill is the source. Run this after changing it, and commit what it writes.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SKILL="$ROOT/skills/adopt-agent-pulse/SKILL.md"
RULE="$ROOT/cursor/adopt-agent-pulse.mdc"

description=$(sed -n 's/^description: //p' "$SKILL" | head -n1)
mkdir -p "$(dirname "$RULE")"
{
  echo "---"
  echo "description: $description"
  echo "alwaysApply: false"
  echo "---"
  # Everything after the skill's own front matter.
  awk 'f>=2 {print} /^---$/ {f++}' "$SKILL"
} > "$RULE"
echo "wrote ${RULE#"$ROOT"/}"
