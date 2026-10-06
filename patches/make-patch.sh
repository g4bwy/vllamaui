#!/usr/bin/env bash
# Regenerate patches/frontend-vllm.patch from the current frontend/ tree.
#   ./patches/make-patch.sh /path/to/llama.cpp/tools/ui
#
# The patch is what ./build.sh --from re-applies after a fresh extraction, so
# run this again whenever you edit anything under frontend/src.
set -euo pipefail

SRC="${1:?usage: make-patch.sh /path/to/llama.cpp/tools/ui}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

[[ -d "$SRC/src" ]] || { echo "error: $SRC is not a tools/ui checkout" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cp -a "$SRC/src" "$TMP/src"
(
	cd "$TMP"
	git init -q .
	git config user.email patch@invalid
	git config user.name patch
	git add -A
	git commit -qm upstream
)

rm -rf "$TMP/src"
cp -a "$DIR/frontend/src" "$TMP/src"

(
	cd "$TMP"
	git add -A
	git diff --cached
) >"$DIR/patches/frontend-vllm.patch"

echo "wrote patches/frontend-vllm.patch: $(grep -c '^diff --git' "$DIR/patches/frontend-vllm.patch") files"
