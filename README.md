# llama.cpp webui for vLLM and Strata

The llama.cpp chat frontend, extracted from `tools/ui`, running against an
OpenAI-compatible server that is not llama.cpp. One Node process serves the built
UI and translates the llama.cpp HTTP contract into what the backend speaks. Two
backends are supported: **vLLM** and **Strata**. Throughput numbers come from the
engine whenever the engine reports them, and from a clock the adapter measures
otherwise. No number in the UI is invented.

Tested against vLLM 0.30.1rc1 serving `qwen3.8-flash-next-fp8`, with 1M context,
prefix caching and speculative decoding enabled, and against Strata
(`github.com/Niko1221/Strata`) running its scripted mock engine.

## Quick start

Tell the adapter where the backend is. Machine-specific values go in `.env`, which
git ignores:

```sh
cp .env.example .env   # then set UPSTREAM_URL
./run.sh -d
```

The backend is detected at startup. Set `BACKEND` to skip that. A variable in the
environment overrides the file, so a one-off run needs no file at all:

```sh
BACKEND=strata UPSTREAM_URL=http://127.0.0.1:8080 ./run.sh -d
```

Open `http://localhost:8080/`. `PORT` changes it. `./run.sh` without `-d` keeps the
process in the foreground, `./run.sh --status` reports the running backend, and
`./run.sh --stop` stops it.

The prebuilt UI ships in `dist/`. To rebuild it after you change the frontend
source, run `./build.sh`. To refresh the source from a llama.cpp checkout, run
`./build.sh --from /path/to/llama.cpp/tools/ui`.

## Which server do I run

There are two implementations of the same job, and they speak the same HTTP to
both the browser and the backend:

- `server-go/` is Go. It is the one to run, and it is the only one that does tool
  calling and MCP. Build it once, then run a single static binary.
- `server/adapter.mjs` is Node. It needs no build step and no toolchain, so it
  stays as the quick path and as the reference the Go port was measured against. It
  answers `GET /tools` with an empty list and 501s `/cors-proxy`, which is what
  turns the webui's tool panel off.

Both read the same `.env`, serve the same `dist/`, and answer the same routes. On
one live vLLM server, `/props` and `/vllm/stats` came back identical apart from the
sample time, the rates window, and `cors_proxy_enabled`, which differed because the
Go instance had been started with `--ui-mcp-proxy`.

### The Go server

```sh
cd server-go
go build -o webui ./cmd/webui
./webui --upstream http://127.0.0.1:8000 --tools=read_file,file_glob_search,grep_search,get_info
```

One dependency: `github.com/modelcontextprotocol/go-sdk`, the official MCP Go
SDK, used only by `internal/mcpx`. Everything else is the standard library.

| Flag | Environment | Meaning |
| --- | --- | --- |
| `-backend` | `BACKEND` | `auto`, `vllm` or `strata`. auto reads `/health` then `/version`. |
| `-upstream` | `UPSTREAM_URL`, `VLLM_UPSTREAM` | Backend base URL. |
| `-port`, `-host` | `PORT`, `HOST` | Listen address, 8080 on all interfaces by default. |
| `-api-key` | `UPSTREAM_API_KEY`, `VLLM_API_KEY` | Bearer token sent to the backend. |
| `-require-api-key` | `API_KEY` | Key callers must present for `/tools`. See below. |
| `-tools` | `TOOLS` | Comma list of built-in tools, or `all`. Empty turns `/tools` off. |
| `-mcp-servers-config`, `-mcp-servers-json` | `MCP_SERVERS_CONFIG`, `MCP_SERVERS_JSON` | The two MCP sources, usable together. |
| `-ui-mcp-proxy` | `UI_MCP_PROXY` | Serve `/cors-proxy` for MCP servers reached from the browser. |
| `-cors-origins` | `CORS_ORIGINS` | Origins allowed on the proxy. Defaults to the UI's own localhost origin. |
| `-dist` | `UI_DIST` | Built UI to serve. |

`-help` lists the rest, including the timing and probe switches the Node server
has.

### Tools, and the warning that goes with them

`--tools` accepts the seven names llama-server has: `read_file`,
`file_glob_search`, `grep_search`, `exec_shell_command`, `write_file`, `edit_file`
and `get_info`, or `all`. Names, JSON schemas, descriptions, parameter defaults,
the `16384` byte read cap, the `32 MiB` base64 cap, the `100` result limits, the
`x-tool-cwd` and `x-resp-type` headers, the `[exit code: N]` tail, the flat error
bodies and the `data: {"chunk":...}` streaming frames all follow llama-server, so
the webui treats this server as if it were llama-server. `--tools-runtime` is
accepted and refused rather than run on the host, because quietly ignoring a
container or ssh target would execute commands somewhere the operator did not ask
for.

Read this before enabling the write tools. Like llama-server, this server does not
confine anything: paths are cleaned but not jailed, `exec_shell_command` runs `sh
-c` with the server's own environment and privileges, and `write_file` and
`edit_file` can change any file that user can reach. The webui asks you to approve
each call, which protects you from the model, not from a browser that can reach
this port. So:

- `read_file` on a machine with secrets on it discloses those secrets to whoever
  can use the chat.
- Bind to `--host 127.0.0.1`, or put it behind a proxy with access control.
- Set `-require-api-key` when anything else may reach the port. The key guards
  `/tools` and `/cors-proxy`, and the two accepted header forms match
  llama-server: `Authorization: Bearer <key>` and `X-Api-Key: <key>`.
- Prefer the read-only list above, and add `exec_shell_command` only when you need
  it. `all` includes the write and shell tools.

The browser has its own working directory per conversation, sent as `x-tool-cwd`.
The picker only appears when at least one enabled tool declares `uses_cwd`.

### MCP servers

Configuration is the Cursor-shaped file llama-server reads, so an existing
`mcpServers` block works unchanged. `examples/mcp-servers.json` shows two entries.

```json
{
  "mcpServers": {
    "files": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/you/projects"],
      "timeout_ms": 30000
    }
  }
}
```

Each server starts as a child process speaking NDJSON JSON-RPC over stdio, is
listed once during a warmup capped at 10 seconds, and appears to the model as
`<server>_<tool>`. A tool whose prefixed name collides with a built-in tool is
skipped with a log line, so a built-in never gets shadowed. A call sends the bare
name. Text parts are joined into `plain_text_response`; `isError` becomes
`error`. Image, audio and resource parts are dropped the way llama-server drops
them, with one log line counting what was discarded, because an empty tool result
is otherwise impossible to debug. A dead child is respawned, and a server that
fails to start rests for five seconds, during which calls answer `MCP server
unavailable: <name>`.

A server entry may instead carry `"url"` to speak streamable HTTP. That is an
addition over llama-server, which is stdio only, and it shares the same lifecycle.

The webui also connects to MCP servers from the browser itself, over HTTP, SSE or
WebSocket. Those need no server configuration. When one is CORS-limited, the
browser goes through `/cors-proxy`, which exists only with `--ui-mcp-proxy`. The
proxy forwards only headers named `x-llama-server-proxy-header-<name>`, renamed,
so cookies, this server's key and any `Authorization` cannot leak to a third-party
host, and it drops URL credentials and bounds the exchange.

### Tests

The Go server carries the test suite the Node one never had.

```sh
cd server-go
go test -race ./...              # about 200 tests across six packages
go test -race ./internal/mcpx/...  # spawns the fixture MCP server as a child
```

`internal/toolsapi`, `internal/builtin` and `internal/mcpx` are unit and
integration tested offline, including the SSE frame shapes, the timing attribution
guards, path escape attempts against `dist`, header precedence on `/tools`, tool
timeout and respawn, and that the proxy cannot leak a cookie. `tests/tools-e2e.mjs`
drives the whole loop in a real browser: the model asks for `read_file`, the UI
prompts for approval, the tool runs over `POST /tools`, and the answer must contain
what the file said.

## Two backends, one webui

`server/adapter.mjs` owns the HTTP server, the static files, the SSE framing, the
cancellation and the caching. `server/backends.mjs` owns everything that differs
between engines. In Go the same split is `internal/core` plus `internal/chat`
against `internal/backend`, and each backend answers the same seven questions:

| Question | vLLM | Strata |
| --- | --- | --- |
| `describe` | build `/props` out of `/v1/models` and `/version` | pass upstream `/props` through, it is already the right shape |
| `translate` | drop llama.cpp-only fields, drop `min_p` and `logit_bias` | drop llama.cpp-only fields, keep `min_p` |
| `prepare` | ask for `usage` and `token_ids` on the stream | send nothing extra, the stream already carries both |
| `rewriteChunk` | rename `delta.reasoning` to `delta.reasoning_content`, count `token_ids` | count characters, the field name is already right |
| `finalTimings` | diff Prometheus histograms around the request | use the engine's own `timings` object |
| `snapshot` | scrape and parse Prometheus text | read JSON from `/metrics` and `/v1/status` |
| `probes` | ask whether the engine thinks and sees | not needed, `/props` says |

Detection is one request each way: Strata answers `/health` with
`"service": "strata"`, and vLLM answers `/version`. If neither replies, the
adapter stays with vLLM, which is the older behaviour.

Both engines get the same `/vllm/stats` JSON, so the frontend has one code path.
The payload carries its own `title` and row labels, which is why a Strata server
reports "VRAM" and "Prompt read" where vLLM reports "KV cache" and "First token".
`/engine/stats` is an alias of the same route.

## What the adapter does

The frontend talks to same-origin relative paths only, so the adapter serves the
UI and the API from one port. The browser needs no CORS setup, since both come
from the same origin.

| Frontend call | Adapter behavior |
| --- | --- |
| `GET /props` | From the backend's `describe`. Reports `role: "model"`, so the UI stays out of router mode. |
| `GET /v1/models` | Passed through. |
| `POST /v1/chat/completions` | Filters the request, streams the answer back, rewrites chunks, and adds timings. |
| `GET /slots`, `GET /tools`, `POST /v1/streams/lookup` | `[]`. The UI reads all three as idle or empty. Strata has a real `/slots`, and the adapter answers the UI itself either way. |
| `POST /models`, `/models/load`, `/models/unload`, `DELETE /models` | 501. Router mode is off, so the UI never calls them. |
| `GET /vllm/stats`, `GET /engine/stats` | Extra endpoint. Engine gauges, live rates, and averages. |
| `GET /build.json` | The backend's version, shown in the About dialog. |

### Request translation

The UI always sends llama.cpp-only fields. Both backends ignore unknown fields,
but the adapter drops them anyway so the upstream request stays clean:
`return_progress`, `sse_ping_interval`, `timings_per_token`, `samplers`,
`backend_sampling`, `reasoning_control`, the `dry_*`, `xtc_*`, `dynatemp_*`
families, `typ_p`, `mirostat*`, and grammar fields. `n_predict` becomes
`max_tokens`, and `n_predict: 0`, the prompt warm-up call, becomes
`max_tokens: 1`: neither engine has "process the prompt, generate nothing", and
one token is the cheapest answer. A negative `max_tokens` is dropped, which
leaves the limit to the engine.

`reasoning_format: "none"` becomes `chat_template_kwargs: {enable_thinking:
false}`, which both engines read from their own chat template.

vLLM specifics: `min_p` and `logit_bias` are dropped, because that build answers
HTTP 400 for them while speculative decoding is active. Set
`VLLM_KEEP_UNSUPPORTED=1` to send them anyway. Streaming requests gain
`stream_options: {include_usage: true}` and `return_token_ids: true`, which is
how the adapter learns exact token counts. Strata honours `min_p` and needs
neither extra field, so its backend adds nothing.

### Capability probing

vLLM publishes no capability information, so the adapter finds two things out by
asking the engine, once at startup, before the port binds:

```
vision support: yes (engine accepted an image)
thinking support: yes (engine emitted reasoning deltas)
```

Thinking. The UI decides whether a model can think by scanning the chat template
it gets from `/props` (`src/lib/utils/chat-template-thinking-detector.ts:46`).
vLLM never serves that template. The adapter runs one short streamed completion,
and if any `delta.reasoning` arrives, `/props` starts reporting thinking support.
The control reaches the UI, and choosing "off" flows back through the
`enable_thinking` mapping above.

Vision. The UI refuses image files unless `/props` says `modalities.vision`, and
the message is "Images require a vision-capable model"
(`src/lib/utils/modality-file-validation.ts:85`). Worse, `chat.service.ts:1077`
silently strips every `image_url` part from history when the flag is off, so an
upload can also disappear without a word. The adapter probes with one tiny
non-streaming request carrying a 1x1 PNG and `max_tokens: 1`. A 200 means the
engine accepts images. A request that carries an image also updates the flag at
runtime, so a wrong answer self-corrects on the next `/props`.

Strata needs none of this. Its `/props` ships a real chat template and a
`modalities.vision` flag, so its backend passes both through and skips both
probes. The boot log says `thinking support: from /props`. Set
`VLLM_MODALITY_VISION=1` or `=0` to decide for yourself on any backend, and
`VLLM_PROBE_VISION=0` or `VLLM_PROBE_THINKING=0` to skip a probe.

The image wire shape needs no translation. The UI sends
`{ type: "image_url", image_url: { url: "data:image/png;base64,..." } }`, which
both engines accept. Once vision is on, the `pdfAsImage` setting in the UI also
becomes reachable, which renders PDF pages as images and sends them the same way.
Audio and video stay off, because the UI sends those in llama.cpp-only part types.

### Cancellation

The UI stops a generation by closing the HTTP connection. The adapter watches that
event and aborts its own request, which cancels the work on the GPU. This matters:
before it was wired up, closing the browser tab during a long answer let vLLM run
to 8092 tokens with nobody listening.

### Stream rewriting

Three things happen to a streamed answer:

1. The field rename for thinking text, on backends that need it.
2. A `timings` object on every chunk, so the live readout has something to show.
3. A full `timings` object on the last chunk, which drives the per-message
   statistics.

Step 2 and 3 matter most. The UI never measures time in the browser. It shows
tokens per second only from the `timings` object a llama.cpp server attaches to
stream chunks (`src/lib/stores/chat/processing.svelte.ts:55`). Against a plain
OpenAI server that field is absent, the live speed readout stays at zero, and the
per-message statistics block disappears.

- Live chunks carry `predicted_n` and `predicted_ms`, from the running token count
  and the clock since the first token. vLLM reports exact ids per chunk. Strata
  reports none, so the count there is characters divided by four, an estimate that
  the engine's own numbers replace when the answer finishes.
- The last chunk carries `prompt_n`, `predicted_n`, `predicted_ms` and `cache_n`,
  plus `prompt_ms` when a prefill could be pinned to that request. Without it the UI
  leaves out the prompt speed.

An in-band engine error frame is not model output. The adapter logs it and withholds
`[DONE]`, so the UI reports a lost stream instead of showing a cut-off answer as
finished. The same happens when the upstream socket dies.

### Where the final numbers come from

Three sources, in order of trust. Each request logs one line naming the one it used:

```
timings(engine) pp 63 tok 742.0 t/s | tg 900 tok 98.1 t/s | cache 0
```

**Engine, in band.** Strata puts a llama.cpp-shaped `timings` object on the last
chunk, measured by its own clock, and it includes the speculative draft counts.
The adapter passes it through unchanged.

**Engine, from counters.** vLLM reports nothing in the stream, so the adapter reads
`GET /metrics` before and after each request and diffs the Prometheus histograms. A
shared server answers many clients at once, so the diff needs a reason to be yours.
The adapter takes it when no other request can explain the numbers:

- Exactly one prefill and one decode finished during the window, and no counter
  moved backwards, since an engine restart resets them.
- The tokens the engine computed fit inside your own prompt. A foreign request
  longer than your whole prompt is not a match.
- The prefill took no more time than the wait for your first token, and that token
  arrived within three times the prefill plus 250 ms. The 250 ms covers the network
  and the gap between the two scrapes. A first token that came later spent its time
  waiting in a queue, so the prefill on offer is not the one you waited for.

An earlier rule asked for an idle engine instead, with nothing running and nothing
waiting when the window opened. On a box that permanently serves three or four
requests at a time that never happened, and no request was ever attributed.

| Value | vLLM counter | Strata field |
| --- | --- | --- |
| `prompt_ms` | `request_prefill_time_seconds` | `timings.prompt_ms` |
| `predicted_ms` | `request_decode_time_seconds` | `timings.predicted_ms` |
| `prompt_n` | `request_prefill_kv_computed_tokens` | `timings.prompt_n` |
| `cache_n` | `usage.prompt_tokens - prompt_n` | `timings.cache_n` |
| `predicted_n` | `usage.completion_tokens` | `usage.completion_tokens` |

**Counts only.** When the window explains nothing, the adapter sends counts and no
prompt speed. It logs the rate as a dash, so the two cases read differently:

```
timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0
```

A prompt rate from the wall clock is wrong in two ways. The time between the request
and the first token holds queue wait and network as well as the prefill, and
`usage.prompt_tokens` counts tokens the engine reused rather than computed. This
vLLM build answers `prompt_tokens_details: null`, so nothing corrects the second
one. The UI only shows a prompt speed when `prompt_ms` is there, so it drops that
line by itself and keeps the rest.

The counts go anyway, because the UI adds `prompt_n + cache_n + predicted_n` for the
context gauge. There `prompt_n` is the whole prompt and `cache_n` is 0 when the
engine said nothing about reuse. The decode rate stays, since the adapter watched
every token of it arrive.

`prompt_n` counts tokens the engine actually computed, and `cache_n` counts tokens
it reused, whenever the numbers came from the engine. That matches llama.cpp, where
the UI needs the two for the context gauge. Strata names the same split, which is why
it passes straight through.

The `/stats` payload mixes the two kinds as well. `Output` and `Prompt` are rates
measured between two polls of cumulative counters, which both engines publish.
`Prefix cache` is the reuse ratio over all requests. `Requests` is what the engine
says is running and waiting. vLLM reports a KV cache fill; Strata has no paged KV
cache to fill, so the same row shows VRAM use and labels itself.

## Running against Strata

Strata is a llama.cpp-style server, which makes this the smaller translation. Its
default port is 8080, the same as this UI, so move one of them:

```sh
# in the Strata checkout, with a real model
./setup.sh                       # serves http://127.0.0.1:8080
# here
BACKEND=strata UPSTREAM_URL=http://127.0.0.1:8080 PORT=8081 ./run.sh -d
```

What Strata gives you for free, and what to expect:

- `/props`, `/slots`, and a real chat template, so thinking and vision need no guess.
- `timings` in the stream, including draft acceptance, so `timings(engine)` is the
  normal log line and no scrape window is needed around your requests.
- `/metrics` as JSON, not Prometheus text. Its `totals` are cumulative, which is
  what the live rates need.
- One request at a time, with the rest queued. The `Requests` row shows the queue.
  Because of this the live rates are the whole engine, not one conversation.
- HTTP/1.0 with no keep-alive: a stream ends by closing the connection. The
  adapter's parser handles a final line with no newline, and `: keep-alive`
  comment lines during prefill pass through the parser untouched.
- Nothing that needs a GPU to test. `python3 -m serve.server --engine mock` runs
  the real server with a scripted answer, and this repo's own
  `tests/mock-strata.mjs` covers what that engine leaves out.

## Frontend changes

The extracted frontend keeps upstream files in place. Three files were added and
five were edited. To see the whole change set:

```sh
diff -rq --exclude=node_modules --exclude=dist --exclude=.svelte-kit \
  /path/to/llama.cpp/tools/ui frontend/
```

The same change set is stored as `patches/frontend-vllm.patch`, which is what makes
a re-extraction safe: `./build.sh --from /path/to/llama.cpp/tools/ui` replaces
`frontend/src` with upstream code, then re-applies that patch, and stops with an
error if upstream moved the files under it. After you edit anything under
`frontend/src`, regenerate the patch with
`./patches/make-patch.sh /path/to/llama.cpp/tools/ui`.

The addition is one block at the bottom of the context gauge popup, reached by the
dial next to the model badge while a chat is active. It shows cache or VRAM
pressure, live output and prompt throughput for the whole engine, requests running
and waiting, prefix cache reuse, speculative decode acceptance, and the average
prompt read. Files:

- `src/lib/services/vllm.service.ts` and `src/lib/stores/vllm.svelte.ts`
- `src/lib/components/app/chat/ChatForm/ChatFormContextGauge/VllmEngineStats.svelte`
- one `$effect` and one `{#if}` in `ContextGaugePopup.svelte`
- `ApiVllmStats` in `src/lib/types/api.d.ts`, plus export lines, plus one
  constant in `src/lib/constants/ui.constants.ts`

The block polls `/vllm/stats` every 3 seconds. A real llama.cpp server answers 404
on that path, so the block stays hidden and the poller stops. A row whose value is
unknown hides itself rather than showing zero, which is why the rate rows appear a
few seconds after the page loads: the first poll has no earlier sample to compare
against.

## Failure modes

- If the backend is unreachable, `/props` answers 503. The UI reads that as
  "backend not ready": it shows a spinner and retries once a second, so the page
  recovers on its own when the backend comes back. Static files keep serving.
- If a completion fails upstream, the error body passes through with its own
  status, so the UI shows the real reason.
- If the stats read fails, timings fall back to the clock and the request still
  succeeds. Nothing in the chat path depends on `/metrics` for an answer.
- An unknown `BACKEND` value stops the adapter at startup with a list of the two
  valid ones, rather than guessing.

## Test it yourself

`e2e.mjs` drives the UI in headless Chromium: it sends a message, reads the live
tokens per second during streaming, opens the context gauge, dumps the engine
block, and reports any console, page, or HTTP error.

```sh
./run.sh -d
node e2e.mjs        # needs the chromium build that npx playwright install adds
UI_URL=http://localhost:8081/ node e2e.mjs     # against another instance
```

Screenshots land in `shots/`. A passing run prints a live tokens-per-second
reading during streaming, per-message prompt and generation speed after it, the
engine rows from the popup, and `(none)` under problems.

`tests/vision-e2e.mjs` does the same for image input: it attaches a real PNG
through the UI file picker, sends it, and fails unless the request carried the
image and the answer repeats text printed inside it. Point it at another picture
with `TEST_IMAGE=/path/to.png TEST_EXPECT=SOMEWORD`. Against a scripted backend,
which answers with fixed text, set `TEST_EXPECT_ANSWER=0` to test the wire only.

## Test without a GPU

`tests/mock-vllm.mjs` and `tests/mock-strata.mjs` are small stand-ins that answer
the way their engine does, including the differences that matter: vLLM with no
in-band timings and a Prometheus `/metrics`, Strata with in-band timings, a real
`/props`, JSON `/metrics`, SSE keep-alive comments, and an error frame.

```sh
node tests/mock-vllm.mjs 8011 &
VLLM_UPSTREAM=http://127.0.0.1:8011 PORT=8012 node server/adapter.mjs

node tests/mock-strata.mjs 8121 &
BACKEND=strata VLLM_UPSTREAM=http://127.0.0.1:8121 PORT=8097 node server/adapter.mjs
```

Both mocks take switches and both advance their counters, so the two timing
sources are testable offline: run one request and read the `timings(engine)` or
`timings(wall)` tag in the log to see which one produced the number. A vLLM line with
a dash where the `pp` rate should be says the window around that request explained
nothing. A proxy that reused a cached scrape for both ends of its window would show
up here as `timings(wall)`, which is how that bug was caught. Send `"truncate": true`
to drop the stream mid-answer, which is how to see the lost-stream path. Strata's
mock also takes `"error_mid_stream": true` and `"no_timings": true`, and reads
`MOCK_VISION=1` and `MOCK_API_KEY`. `tests/restart-mock.sh [port]` restarts the Strata
mock by pid file.

To test against Strata's own code rather than a stand-in, run its real server with
its scripted engine. It needs no GPU and no extra Python packages:

```sh
cd /path/to/Strata && python3 -m serve.server --engine mock --port 8095
```

That engine answers instantly and keeps no clock of its own, so it exercises the
`/props` passthrough, the field names, and the wall-clock fallback, and it reports
no speculative numbers. The mock in `tests/` exists because of those gaps.

## Security

The adapter has no authentication of its own, and it forwards `UPSTREAM_API_KEY` to
the backend. It listens on all interfaces by default. Bind it to a trusted network,
put it behind your usual reverse proxy with access control, or run it with
`HOST=127.0.0.1`.

Responses sent to the browser carry no backend address: `/props` reports the
engine version only, and `/stats` reports engine numbers. The upstream URL and any
key live in `.env`, which git ignores, and appear in the local boot log.

## Configuration

Set these in `.env`, or in the environment, which takes precedence. The adapter
reads `.env` from the project root at startup and prints the resolved backend in
its boot log.

| Variable | Default | Purpose |
| --- | --- | --- |
| `BACKEND` | `auto` | `vllm`, `strata`, or `auto`, which asks the upstream. |
| `UPSTREAM_URL` | `http://localhost:8000` | Backend base URL. `VLLM_UPSTREAM` is the same thing. |
| `PORT` / `HOST` | `8080` / `0.0.0.0` | Where the adapter listens. |
| `UPSTREAM_API_KEY` | empty | Sent as `Bearer` to the backend. `VLLM_API_KEY` also works. |
| `VLLM_MODEL` | first model reported | Model id to send when the UI omits it. |
| `VLLM_N_CTX` | from the backend | Context size shown by the gauge. |
| `VLLM_ENGINE_TIMINGS` | `1` | vLLM only. `0` skips the metrics window and times by wall clock. |
| `VLLM_MODALITY_VISION` | auto-detect | `1` forces image input on, `0` forces it off. |
| `VLLM_PROBE_VISION` | `1` | vLLM only. `0` skips the startup image probe. |
| `VLLM_PROBE_THINKING` | `1` | vLLM only. `0` skips the startup thinking probe. |
| `VLLM_KEEP_UNSUPPORTED` | `0` | vLLM only. `1` sends `min_p` and `logit_bias`. |
| `UI_DIST` | `./dist` | Built frontend to serve. |

`VLLM_` stays in the names that were there first, to avoid breaking a working
`.env`. The three probe switches and the timings window have no effect on Strata,
which reports those things itself.

## Known gaps

- No live prompt processing progress bar. llama.cpp sends `prompt_progress` while
  it reads a prompt. Neither backend reports per-request progress that the adapter
  could put in that field, so there is no measured number to show. Prompt speed
  appears when the answer completes. Strata's `/metrics` has `live.prompt_read`
  and `live.prompt_total` for its own Monitor, which is close, and the adapter does
  not use them.
- Live tokens per second is an estimate on Strata, from characters, because that
  stream carries no token ids. It is replaced by the engine's measured numbers on
  the last chunk. vLLM counts exact tokens throughout.
- No stream resume. llama.cpp replays a dropped stream from a byte offset via
  `/v1/stream`. The adapter ends every normal stream with `[DONE]`, so that path
  never comes up. When the upstream dies mid-token the adapter closes without
  `[DONE]` on purpose, and the UI reports "Stream connection lost" while keeping
  the partial answer.
- No server-side tools. llama.cpp exposes `/tools` for file and shell access. The
  UI shows an empty list, and MCP tools configured in the browser still work.
- The stats block is engine-wide, not per conversation. On Strata, which serves one
  request at a time, those are nearly the same thing. On a shared vLLM they are not.
- Chat template on vLLM is unknown. vLLM does not serve it, so `/props` reports the
  probe marker instead, and the UI cannot inspect the real template. Reasoning
  streams and displays either way.

## Layout

```
server-go/            the Go server: cmd/webui plus internal/{appconf,backend,chat,
                      core,builtin,mcpx,toolsapi,contracts}
examples/             sample mcpServers configuration
server/adapter.mjs    HTTP server, static files, SSE plumbing, backend-agnostic
server/backends.mjs   what differs between vLLM and Strata
frontend/             extracted llama.cpp webui source plus the vLLM additions
dist/                 built frontend, what the adapter serves
build.sh              rebuild dist/ from frontend/, optionally re-extract first
run.sh                start, stop, and status for the adapter
e2e.mjs               headless browser test of the running UI
tests/tools-e2e.mjs   browser test of a real tool round trip through the model
tests/mock-vllm.mjs   fake vLLM server
tests/mock-strata.mjs fake Strata server, including the parts its own mock omits
tests/vision-e2e.mjs  browser test that an attached image reaches the model
tests/restart-mock.sh restart a mock by pid file
patches/              the frontend change set, plus the script that makes it
.env                  your backend host and key, never committed
.env.example          template for it
var/                  background log and pid files
shots/                screenshots written by the browser tests
```

`frontend/` carries the upstream `README.md` and the CMake files from `tools/ui`.
Ignore both: they describe the llama.cpp build, which this project does not use.

Requirements: a reachable backend, plus Node 18 or newer for the Node server and
its browser tests (tested on Node 24), or Go 1.25 or newer for the Go server
(tested on 1.26).

## Provenance

`frontend/` is a copy of the llama.cpp webui, taken from `tools/ui` in
ggml-org/llama.cpp at commit `d7a695ef679138c13d86359b84c1731d36213d32`. That code
is MIT licensed, the license text is in `LICENSE.llama.cpp`, and `frontend/README.md`
is the upstream document, describing the llama.cpp build that this project does not
use. Everything under `server/`, `tests/`, and `patches/` was written for this
port. Strata is a separate project at `github.com/Niko1221/Strata`, also MIT, and
this repository contains none of its code, only a client for its API.

To move to a newer llama.cpp, run `./build.sh --from <path>/tools/ui`. It extracts,
re-applies the vLLM patch, and rebuilds. If the patch no longer applies, the
command names the files upstream moved.
