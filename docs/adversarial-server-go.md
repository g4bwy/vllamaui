# Adversarial review: `server-go/`

Date: 2026-10-06. Scope: all Go code under `server-go/` (about 7,100 non-test lines).
Method: four parallel read-only review agents (core/cmd, backend, builtin tools,
mcpx/toolsapi). Each agent traced hostile inputs through the real code, then the
main agent re-verified every Critical and High finding line by line. The agents
reproduced the confirmed findings in throwaway copies under `/tmp`. The repo was
not modified during the review.

`go build` and `go vet` are clean. The full test suite passes under `-race`.

## Status

As of `9f4ff7e`, re-verified against the current tree on 2026-10-06.

Fixed:

- Findings 1, 2, 3 (all Critical), in commits `d433d7b` and `1cf2c83`. See
  the remediation log at the end.
- Two side effects: the `read_file` TOCTOU Low (fixed with finding 1) and the
  `api-key` header mismatch Low (gone with the CORS change).

Todo:

- High findings 4 to 9.
- All Medium items. One is now half-done: `read_file` honors its request
  context, `edit_file` still discards it.
- The remaining Low items.

Line numbers below are as of the review tree. The fixes and the concurrent
vLLM timings rewrite shifted some references in `core.go`, `vllm.go`,
`backend.go`, `main.go`, and `paths.go`; the moved ones are marked inline.

## Background that shapes severity

`internal/builtin/paths.go:13-14` states explicitly that tool paths are not
confined to the working directory. This is a deliberate mirror of llama-server.
There is no sandbox to escape. Everything exposed under `--tools` runs with the
full file and process privileges of the server user.

Three of the four worst findings (1, 2, 3) come from the same decision
compounding. This port mirrors llama-server, which assumes a localhost-only,
fully trusted process. The defaults (`HOST=0.0.0.0`, no inbound key, wildcard
CORS on every route) quietly drop that assumption, while the mirrored behaviors
(unconfined tool paths, unauthenticated relay, no input caps) keep it.

---

## Critical

### 1. One tool call can kill the whole server: unbounded read of size-0 pseudo files

Status: fixed in `d433d7b`.

Location: `internal/builtin/readfile.go:26,45-50`

`os.Stat` reports size 0 for `/dev/zero`, FIFOs, and many device nodes, so the
`readFileMaxSize` gate passes, then `os.ReadFile` allocates without limit.
`read_file {"path":"/dev/zero"}` ends in `fatal error: runtime: out of memory`.
A Go runtime fatal error is not caught by the `recover()` in
`internal/toolsapi/toolsapi.go:272`, so every connection drops. A FIFO in the
working directory instead hangs the handler forever, because `readFile` ignores
its context (`readfile.go:13`).

The size gate is also skipped whenever `end_line` is given
(`readfile.go:45`, condition `&& endLine == -1`). Confirmed: reading one line of
a 125 MB file with `end_line=1` allocated 241 MB.

Fix direction: require `info.Mode().IsRegular()`, and stream through
`io.LimitReader` instead of `os.ReadFile`.

### 2. The tool endpoint is a remote file-read and RCE surface that is unauthenticated when enabled

Status: fixed in `1cf2c83`.

Location: `internal/appconf/appconf.go:212,220`, `cmd/webui/main.go:162-164`,
`internal/core/core.go:145-153`, `internal/builtin/exec.go:41`

The chain, confirmed by execution: `--tools all` enables `exec_shell_command`
(`sh -c`, no allowlist) and the unconfined file tools. `HOST` defaults to
`0.0.0.0`. `InboundKey` defaults to empty, so `api.Keys(...)` is never called
and `/tools` has no gate. A `text/plain` POST from any web page the user visits
is a CORS-simple request that needs no preflight, and the router stamps
`Access-Control-Allow-Origin: *` on every response, so the attacker page also
reads the result. A cross-origin `POST /tools` ran `echo pwned-by-browser` and
returned its stdout.

Fix direction: bind `127.0.0.1` by default. Require an inbound key whenever a
write or exec tool is enabled, unless explicitly waived. Replace the wildcard
CORS with per-route decisions.

### 3. `/cors-proxy` is an unauthenticated SSRF relay with no target restrictions

Status: fixed in `1cf2c83`.

Location: `internal/toolsapi/proxy.go:59,91,152-176,235-246`,
`cmd/webui/main.go:169-176`

`appconf.go:53-55` documents that `InboundKey` guards `/tools` and
`/cors-proxy`, but `main.go` gates only `/tools`. The proxy has no key hook at
all. `proxyTarget` accepts any http(s) host, including `127.0.0.1` and
`169.254.169.254`, and the default `http.Client{}` (`proxy.go:46`) follows
redirects, so an external URL can retarget the request into the local mesh.

The origin allowlist is dead code for two reasons. `cors()` only sets a header
for allowed origins and never removes the `*` that `core.go:153` wrote first,
and the blanket OPTIONS branch (`core.go:145`) answers the preflight before the
route logic runs. CORS never stopped non-browser callers anyway. `curl` with no
`Origin` header got the internal body back with status 200.

Fix direction: wire the same key gate into the proxy route. Remove the global
wildcard CORS and the global OPTIONS answer, and let the proxy manage its own
headers with its allowlist. Refuse link-local and cloud-metadata targets.
Return upstream redirects to the caller instead of following them.

---

## High

### 4. Chunks carrying `usage` are withheld, so per-chunk usage stats silently truncate the answer

Status: open. `chat.go:228-233` unchanged.

Location: `internal/chat/chat.go:228-233`

Any upstream chunk with a non-null `usage` is stored in `pending` and never
forwarded. vLLM supports `stream_options: {"continuous_usage_stats": true}`,
which puts `usage` on every chunk. Confirmed: a four-chunk reply arrived as one
frame that held only the last delta.

Fix direction: hold only chunks that carry no delta content. Otherwise forward
the chunk and attach timings to a synthetic final frame.

### 5. `exec_shell_command` timeout is bypassable

Status: open. `drain` still has no post-cancel deadline and no `WaitDelay`.

Location: `internal/builtin/exec.go:82-108`

`runProc` kills the process group on timeout, then `drain` waits for EOF on the
output pipe. A grandchild detached with `setsid` inherits the write end and
keeps it open, so EOF never arrives and `cmd.Wait` is never reached. Confirmed:
`{"command":"setsid sleep 12 & echo started; exit 0","timeout":1}` returned
after 12.0 s and reported both `[exit code: 0]` and `[exit due to timed out]`.

Fix direction: bound `drain` with a deadline after cancel, and close the read
end when the alarm fires.

### 6. Every child process inherits the server's secrets

Status: open. `exec.go` still never sets `cmd.Env`; `transport.go:72-74` still
starts from `os.Environ()`.

Location: `internal/builtin/exec.go:76-92` (never sets `cmd.Env`),
`internal/mcpx/transport.go:73,86-104` (`childEnv` starts from `os.Environ()`)

Confirmed: `exec_shell_command {"command":"env"}` printed
`UPSTREAM_API_KEY=...` and `AWS_SECRET_ACCESS_KEY=...` into the model's context,
from where they are easy to exfiltrate. The same full environment goes to every
spawned third-party MCP server, including packages fetched from PyPI at start
time.

Fix direction: default to a small environment allowlist (`PATH`, `HOME`,
`LANG`, `TMPDIR`) plus explicit `env` entries from the MCP configuration.

### 7. Revoked MCP tools stay callable; the advertised list is frozen at boot

Status: open. `Merge` is still called once at startup (`main.go:164`).

Location: `internal/toolsapi/merge.go:14-34`, `internal/mcpx/client.go:195-226`

`Merge` copies tool pointers once at startup; its own doc comment says the
snapshot does not follow later changes. The MCP client does re-list on
`notifications/tools/list_changed`, but nothing rebuilds the merged registry.
Confirmed: after a server revoked tool X, `GET /tools` still listed it and
`POST /tools {"tool":"X"}` still executed on the server; a newly added tool Y
returned 404 until restart.

Fix direction: make the merged registry forward `List`/`Get` to its owners
live, with first-wins resolution per call.

### 8. Unbounded body reads and no server timeouts on unauthenticated routes

Status: open. Still no timeouts on `http.Server` (`core.go:98`) and no
`http.MaxBytesReader` anywhere; only the chat route caps its body.

Location: `internal/core/core.go:98` (no timeouts on `http.Server`),
`internal/toolsapi/toolsapi.go:186` and `internal/toolsapi/proxy.go:91`
(`io.ReadAll(r.Body)` with no cap)

Confirmed: one 64 MB unauthenticated `POST /cors-proxy` grew the heap by 222 MB
(about 3.5x amplification), and 128 concurrent 600 s relays all stayed in
flight. A few parallel requests exhaust memory; a slow-loris client pins a
goroutine forever.

Fix direction: wrap bodies in `http.MaxBytesReader`, set
`ReadHeaderTimeout`/`IdleTimeout`, and bound concurrent relays.

### 9. Uncancellable regex blowup in the glob matcher

Status: open. Now at `paths.go:68-116` (references shifted by the readfile
commit's neighborhood, the matcher itself is unchanged).

Location: `internal/builtin/paths.go:68-116`, `internal/builtin/globsearch.go:55-63`

`globMatchAt` is backtracking recursion with no memo, and matching runs after
the walk, outside the 15 s walk deadline. Confirmed: the include pattern
`*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*ab` against one 34-char filename took 23.1 s
with 16 pairs, doubling per two pattern characters; 40 pairs never returns.

Fix direction: add a step budget or a linear-time matcher, and check the
deadline inside the match loop.

---

## Medium

Status: all items open, except the context note on the last one.

- `grep_search` and `edit_file` read whole files with no size cap
  (`grepsearch.go:57,86`, `editfile.go:62`). A 6 GB sparse file in a searched
  tree caused 12 GB of allocation in one call. Same class as finding 1.
- `grep_search` output has no byte cap and `context_lines` is unbounded
  (`grepsearch.go:29-32,100-123`). A 1.2 MB file produced 14.5 MB of output
  with `context_lines=1000000000`.
- 502/503 bodies echo the private upstream URL to any cross-origin reader
  (`core.go:165,235`, `chat.go:90-91`; refs after the CORS commit). Confirmed
  with a URL that carries
  `user:pass@`: the credentials came back in the response body. This violates
  the invariant stated at `core.go:277` and enforced by `safeBuildInfo`. Log
  the detail, answer generically, and use `URL.Redacted()`.
- No idle bound on the streaming read, and no deadline on the upstream client
  (`backend/backend.go:66` uses a bare `&http.Client{}`, `chat.go:249-278`).
  Confirmed: an upstream that accepts the TCP connection and goes quiet pins
  the handler, the browser connection, and the upstream connection
  indefinitely. Set `ResponseHeaderTimeout` and renew a read deadline per
  chunk.
- A non-SSE upstream 200, or an in-stream engine error frame, becomes a silent
  empty stream (`chat.go:192-245,288-301`). The browser sees HTTP 200 with no
  tokens; a mid-stream `"CUDA out of memory"` error frame is logged and
  dropped. Relay the payload or emit a final error frame carrying `st.Err`.
- The vision-capability flag is global and triggered by loose text matching
  (`vllm.go:349`, `chat.go:101-118`; the regex moved when the timings code
  landed). One unrelated 400 whose body contains
  lowercase "image" hides the image control for every user until restart. A
  genuine rejection phrased with capital "Image" fails to disable it, because
  the port dropped the reference implementation's case-insensitive flag.
- An unknown default model forces a full `/v1/models` read per request with no
  negative cache (`backend.go:174-201`; ref moved with the timings rewrite).
  Confirmed: while the engine loads,
  each chat request pays the 8 s models timeout before the completion is even
  attempted, and the UI retry loop multiplies that.
- MCP `timeout_ms: 0`, a negative value, or a huge float breaks every call to
  that server (`config.go:151-153`, `client.go:283`). Confirmed: `0` yields
  "request timed out" on a healthy server; `1e30` overflows to negative;
  `1e18` holds the entry mutex for 63 years. Clamp with a log line.
- The Cursor-compatible MCP config silently drops `disabled` and `headers`
  (`mcpx/config.go:105-112`). Confirmed: a server marked `"disabled": true` is
  still spawned and answers calls; configured auth headers vanish, leaving
  only "MCP server unavailable".
- MCP discovery is serial and blocks port binding (`client.go:110,159-174`,
  `main.go:52-59`). Confirmed: 3 stalled servers delayed `New()` by 5.1 s; at
  production caps, a few dead servers mean about a minute of connection
  refused.
- The Strata live token estimate counts UTF-8 bytes (`strata.go:183-187` uses
  `len(text)`). CJK token counts run 3x high, so the displayed t/s is wrong.
  Use `utf8.RuneCountInString`.
- Writes in `write_file` and `edit_file` are non-atomic and the
  read-modify-write is unlocked (`writefile.go:38-45`, `editfile.go:62-126`).
  A write that fails partway leaves a truncated file; two concurrent sessions
  silently lose one edit. Use a temp file plus rename.
- A symlink inside `--dist` escapes the served root (`static.go:60-65,115`).
  The prefix containment check is correct, and the sibling-prefix case is
  properly rejected, but `os.Stat`/`os.Open` follow links, so one stray
  symlink in the tree exposes any readable file. Resolve with
  `filepath.EvalSymlinks` before the check.
- `appconf` silently keeps defaults for unparseable values (`PORT=not-a-number`
  yields 8080; `UI_MCP_PROXY=yes` stays off; `-engine-timings=banana` stays
  on) and passes `HOST=http://localhost:1234` verbatim to `net.Listen`
  (`appconf.go:173-213`). Security-relevant switches can be silently off.
- A tool request context can be ignored: `read_file` now polls it (fixed with
  finding 1), but `edit_file` still takes `_ context.Context`
  (`editfile.go:25`), so a blocked edit survives client disconnect.

## Low

Status: all open except the two noted in the Status section at the top.

- `--api-key` guards only `/tools`, not `/v1/chat/completions`, so any page
  that reaches the port can spend the upstream key and GPU time
  (`appconf.go:55`). The wildcard-CORS half of this finding is gone (fixed
  with finding 3), which shrinks the practical exposure to same-origin and
  non-browser callers. Operators who read the flag the way llama-server does
  are still surprised.
- The relay lets callers set arbitrary upstream headers through
  `x-llama-server-proxy-header-*`, including `Authorization`
  (`proxy.go:182-197`). Confirmed: the upstream received a spoofed admin
  header. Request smuggling does not work, because Go drops a caller-set
  `Transfer-Encoding`.
- The upstream bearer key is replayed on same-host cross-port redirects
  (`backend.go:66`: Go compares hosts and ignores the port). Confirmed.
- A relay deadline answers as an empty 200 instead of 504 (`proxy.go:109-119`).
- `<server>_<tool>` namespacing collides (`client.go:453`): servers `a` and
  `a_b` can each hold `b_c`/`c`; the second tool silently disappears.
- Method is not checked on most API routes (`core.go:155-204`): `POST /props`,
  `PUT /slots`, and `DELETE /v1/stream` (which answers `{"success":true}` for
  any id) are all served.
- `authorized` compares the key with `==` (`main.go:313,316`; ref moved with
  the guard commit). The `api-key` header mismatch part of this finding is
  gone: no preflight advertises the header any more.
- `--dist=""` makes `filepath.Abs("")` the process working directory, so every
  working-directory file with an extension is downloadable
  (`appconf.go:214`, `static.go:53`).
- `LoadDotEnv` calls `os.Setenv` for API keys, which then flow into all child
  processes (`appconf.go:89-108`); this compounds finding 6.
- `strata.Snapshot` returns `(nil, nil)` on failure while its stats readers
  dereference the snapshot (`strata.go:219-221,309,356`). Unreachable today,
  and a panic waiting for the next caller.
- The live timing object overwrites the engine's own timings on the finish
  frame (`chat.go:212-243`); the engine's prompt-side numbers vanish from that
  frame.
- `Permissions.Write` is hardcoded `false` for every MCP tool
  (`client.go:510`). The `readOnlyHint`/`destructiveHint` annotations are never
  read. No in-tree consumer trusts the field today, but the published field
  lies.
- `read_file` had a stat-then-read TOCTOU. Fixed with finding 1 (`d433d7b`):
  the base64 path reads through `readCapped` and the text path caps each line
  to the remaining output room, so growth after the stat can no longer exceed
  the caps.
- Raw upstream and transport error text is handed to the `/tools` caller
  (`result.go:127`, `client.go:398-411`), which can surface internal paths or
  child stderr in API errors.

---

## Verified clean

The reviewers tried and failed to break: SSE line framing across split TCP
writes, including splits inside multi-byte runes; response body closing on
every path; cancellation propagation on client disconnect; the `tracker`
double-WriteHeader guard; `edit_file` uniqueness, fuzzy-match, and overlap
logic (traced case by case, and the dropped-edit branch is unreachable);
symlinked-directory descent during the walk (symlinked directories are not
followed); JSON-RPC framing and id correlation (delegated to go-sdk v1.8.0,
which caps frames at 16 MiB); child cleanup and zombie reaping (`Process.Kill`
after `Wait` is a no-op, so no PID-reuse kill); Prometheus parsing (NaN/Inf
rejected, 8 MiB read cap); graceful shutdown releasing the port with a stream
in flight. All shared state was `-race` clean under stress.

## Suggested remediation order (remaining work)

1. The `chat.go` cluster: finding 4, the silent-stream Medium, the dropped
   error frame, and the missing streaming deadline. One file, one pass.
2. The exec cluster: findings 5 and 6, plus the `edit_file` context.
3. Finding 7 with the MCP config Mediums: `disabled`, `headers`,
   `timeout_ms`.
4. Finding 8 with the remaining unbounded reads (grep, edit, relay bodies)
   and the server timeouts, as one hardening pass.
5. Finding 9 and the grep output Mediums.

---

## Remediation log (2026-10-06, same day)

The three Critical findings are fixed, live-verified, and re-verified against
the current HEAD after a concurrent timings commit and a history rewrite
landed on top. Commits: `d433d7b` (finding 1), `1cf2c83` (findings 2 and 3).
The full suite passes: `gofmt`, `go build`, `go vet`,
`go test -race -count=1 ./...`, all ten packages.

### Finding 1: unbounded read (fixed)

`internal/builtin/readfile.go` now requires `info.Mode().IsRegular()` before
either size gate, so `/dev/zero`, device nodes, and FIFOs return
`cannot read file: <path>`. The text path streams through a 64 KiB buffered
reader instead of `os.ReadFile`, buffers at most the remaining output room per
line, checks the request context during the scan, and stops once the range or
the 16 KB cap is reached. The base64 path reads through
`io.LimitReader(f, readFileMaxSizeB64+1)`. Oversized-file wording is unchanged.
Regression tests cover the device, the fifo, bounded memory on a 200 MiB sparse
file (measured 65 KiB allocated), the capped base64 read, cancellation, and
byte-for-byte parity with the old algorithm over 18 shape cases.

### Finding 2: unauthenticated tool endpoint (fixed)

- `appconf.go`: `Host` defaults to `127.0.0.1`; the `-host` help text says why.
- `core.go`: the router no longer stamps `Access-Control-Allow-Origin: *`, and
  the blanket wildcard preflight is gone. Same-origin UI traffic is unaffected;
  only the proxy hands out CORS headers, from its own allow list.
- `cmd/webui/main.go`: `toolsGuard` refuses to start when the registry lists a
  tool with `Permissions.Write` and no inbound key is configured. Verified
  live: `-tools all` alone exits with "refusing to start: the tools
  exec_shell_command, write_file, edit_file can change this machine...". The
  new `--allow-unauthenticated-tools` / `ALLOW_UNAUTHENTICATED_TOOLS` flag
  starts anyway and logs a WARNING that names the tools and the bind address.

### Finding 3: open SSRF relay (fixed)

`internal/toolsapi/proxy.go`:

- `Keys(...)` mirrors the `/tools` gate, checked before any target work;
  verified live, an unkeyed `POST /cors-proxy` returns 401. `main.go` wires it
  when `--require-api-key` is set, which makes the old `appconf` comment true.
- `refusedTargetHost` rejects link-local ranges and cloud metadata endpoints
  (169.254.0.0/16, fe80::/10, fd00:ec2::254, metadata.google.internal).
  Verified live through the running server: keyed request against
  169.254.169.254 returns 400 "target address is not allowed". Loopback
  targets stay allowed: a local MCP server is what the relay exists for.
- The client uses `CheckRedirect: http.ErrUseLastResponse`, so a 3xx returns
  to the caller unfollowed and cannot retarget the fetch.
- `cors()` deletes any inherited `Access-Control-Allow-Origin` before it
  decides, so a disallowed origin provably gets no header.

Tests updated with the behavior: the two router assertions that pinned the
wildcard now assert its absence; the guard matrix, target floor, key gate,
redirect relay, and end-to-end CORS cases are new.

### Not addressed in this round

Findings 4 to 9 and the Medium/Low items stay open; a second sweep of the
current tree on 2026-10-06 confirmed each one still reproduces in code, and
the Status lines mark them. Fixed as side effects: the `read_file` TOCTOU and
the preflight `api-key` mismatch. One note for whoever takes the rest: `mcpx`
publishes `Permissions.Write: false` for every MCP tool, so `toolsGuard`
covers built-ins only until that field reads the real annotations.

### Unrelated concurrent work

A vLLM timings-attribution rewrite (`attributeWindow` in
`internal/backend/vllm.go`, matching edits in `server/backends.mjs`,
`tests/mock-vllm.mjs`, `internal/chat/chat_test.go`, and README prose)
landed as `9558810` during the same day. It does not overlap the security
fixes, and the combined tree passes the full race suite.
