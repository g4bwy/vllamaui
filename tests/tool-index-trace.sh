#!/usr/bin/env bash
# Print the tool_call index sequence for one prompt, straight from the engine and
# then through the proxy, so the batch renumbering is visible.
#   ./tests/tool-index-trace.sh [proxy-url] [tries]
set -u
PROXY="${1:-http://localhost:8085}"
TRIES="${2:-3}"
ENGINE="${ENGINE_URL:-http://fender.lan:8000}"
KEY="${VLLM_API_KEY:-${UPSTREAM_API_KEY:-}}"

REQ=$(cat <<'JSON'
{"messages":[{"role":"user","content":"what is in the news today ? compare two topics in parallel"}],
 "stream":true,
 "tools":[
  {"type":"function","function":{"name":"searxng-mcp_web_search","description":"web search","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}},
  {"type":"function","function":{"name":"fixture_echo","description":"echo text","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}}
 ]}
JSON
)

trace() {
	python3 -c '
import sys, json
seq = []
for line in sys.stdin:
    if not line.startswith("data:"):
        continue
    try:
        d = json.loads(line[5:].strip())
    except Exception:
        continue
    ch = (d.get("choices") or [None])[0]
    if not ch:
        continue
    dl = ch.get("delta") or {}
    if dl.get("content") or dl.get("reasoning_content"):
        seq.append("text")
    for tc in dl.get("tool_calls") or []:
        seq.append("T" + str(tc.get("index")))
print(" ".join(seq) if seq else "(no tool calls)")
'
}

auth=()
[ -n "$KEY" ] && auth=(-H "Authorization: Bearer $KEY")

for i in $(seq 1 "$TRIES"); do
	echo "--- try $i"
	echo -n "  engine $ENGINE  : "
	curl -sN --max-time 90 -H 'Content-Type: application/json' "${auth[@]}" -d "$REQ" "$ENGINE/v1/chat/completions" | trace
	echo -n "  proxy  $PROXY : "
	curl -sN --max-time 90 -H 'Content-Type: application/json' -d "$REQ" "$PROXY/v1/chat/completions" | trace
done
