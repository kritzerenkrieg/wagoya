#!/usr/bin/env bash
# Prints a hash of everything the golden set depends on: the contents of
# every tracked or untracked Go file and go.mod/go.sum (docs don't count, and
# committing doesn't change it), and the model settings in .env. A
# passing full golden run records it in dist/.golden-pass; deploy.sh skips
# the gate when it still matches (nothing changed since).
set -euo pipefail
cd "$(dirname "$0")/.."
{
  git ls-files --cached --others --exclude-standard -z -- '*.go' go.mod go.sum | sort -zu |
    while IFS= read -r -d '' f; do [ -f "$f" ] && sha256sum "$f"; done
  grep -E '^(PROVIDER|MODEL|EFFORT|EMBED_MODEL)=' .env 2>/dev/null || true
} | sha256sum | cut -d' ' -f1
