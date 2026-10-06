#!/usr/bin/env bash
# Rebuild the extracted webui into dist/.
#
# Optional flags:
#   --from /path/to/llama.cpp/tools/ui   re-extract the frontend source first
set -euo pipefail

cd "$(dirname "$0")"
DIR="$PWD"

while [[ $# -gt 0 ]]; do
	case "$1" in
	--from)
		SRC="$2"
		echo "extracting $SRC -> frontend/"
		[[ -d "$SRC/src" ]] || { echo "error: $SRC does not look like tools/ui" >&2; exit 1; }
		mkdir -p "$DIR/frontend"
		# drop src/ first: tar only overwrites, so leftover vLLM files would make
		# the patch refuse to create them again
		rm -rf "$DIR/frontend/src"
		tar -C "$SRC" \
			--exclude=node_modules --exclude=./dist --exclude=./.svelte-kit \
			--exclude=./dev-dist --exclude=./test-results \
			-cf - . | tar -C "$DIR/frontend" -xf -
		# extraction restores upstream files over the vLLM edits, so re-apply them
		if [[ -f "$DIR/patches/frontend-vllm.patch" ]]; then
			echo "re-applying patches/frontend-vllm.patch"
			if ! (cd "$DIR/frontend" && git apply "$DIR/patches/frontend-vllm.patch"); then
				echo "error: the patch did not apply cleanly. Upstream has moved these files:" >&2
				echo "  $(cd "$DIR/frontend" && git apply --numstat "$DIR/patches/frontend-vllm.patch" 2>/dev/null | cut -f3 | tr '\n' ' ')" >&2
				echo "Re-apply the vLLM block by hand, then regenerate the patch with:" >&2
				echo "  ./patches/make-patch.sh /path/to/llama.cpp/tools/ui" >&2
				exit 1
			fi
		fi
		shift 2
		;;
	*)
		echo "unknown flag: $1" >&2
		exit 1
		;;
	esac
done

cd "$DIR/frontend"
if [[ ! -d node_modules ]]; then
	echo "installing dependencies (this can take a few minutes)"
	npm ci --no-audit --no-fund
fi

echo "building"
npm run build

rm -rf "$DIR/dist"
mkdir -p "$DIR/dist"
cp -r "$DIR/frontend/dist/." "$DIR/dist/"
echo "done: $DIR/dist ($(du -sh "$DIR/dist" | cut -f1))"
