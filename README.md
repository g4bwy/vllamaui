# llama.cpp webui for vLLM

The llama.cpp chat frontend, extracted from `tools/ui`, running against a vLLM
server. One Node process serves the built UI and translates the llama.cpp HTTP
contract into the OpenAI API that vLLM speaks. Throughput numbers (tokens per
second for prompt processing and for generation) come from the vLLM engine
counters when the request can be attributed to them, and from measured wall
clock otherwise. Nothing here reports a made-up number.

Tested against vLLM 0.30.1rc1 serving `qwen3.8-flash-next-fp8`, with 1M context,
prefix caching and speculative decoding enabled.

## Quick start

Tell the adapter where your vLLM server is, then start it. Machine-specific
values go in `.env`, which git ignores:

```sh
cp .env.example .env   # then set VLLM_UPSTREAM
./run.sh -d
```

A variable in the real environment overrides the file, so a one-off run needs no
file at all: `VLLM_UPSTREAM=http://your-host:8000 ./run.sh -d`.

Open `http://localhost:8080/`. The port is the `PORT` variable, 8080 by default.
Run `./run.sh` without `-d` to keep the process in the foreground, and
`./run.sh --stop` to stop a background instance.

The prebuilt UI ships in `dist/`. To rebuild it after you change the frontend
source, run `./build.sh`. To refresh the source from a llama.cpp checkout, run
`./build.sh --from /path/to/llama.cpp/tools/ui`.

## What the adapter does

The frontend talks to same-origin relative paths only, so the adapter serves the
UI and the API from one port. The browser needs no CORS setup, since both come
from the same origin.

| Frontend call | Adapter behavior |
| --- | --- |
| `GET /props` | Synthesizes the llama.cpp props object from `/v1/models`, `/version`, and `/metrics`. Reports `role: "model"`, so the UI stays out of router mode. |
| `GET /v1/models` | Passes the vLLM list through unchanged. |
| `POST /v1/chat/completions` | Filters the request, streams the answer back, rewrites chunks, and adds timings. See below. |
| `GET /slots`, `GET /tools`, `POST /v1/streams/lookup` | Returns `[]`. The UI treats all three as idle or empty, which is correct here. |
| `POST /models`, `/models/load`, `/models/unload`, `DELETE /models` | Returns 501. Router mode is off, so the UI never calls them. |
| `GET /vllm/stats` | Extra endpoint. Engine gauges, live rates, and lifetime averages. The UI reads it for the "vLLM engine" block in the context gauge popup. |
| `GET /build.json` | Reports the vLLM version in the About dialog. |

### Request translation

The UI always sends llama.cpp-only fields. vLLM ignores unknown fields in this
version, but the adapter drops them anyway so the upstream request stays clean:
`return_progress`, `sse_ping_interval`, `timings_per_token`, `samplers`,
`backend_sampling`, `reasoning_control`, the `dry_*`, `xtc_*`, `dynatemp_*`
families, `typ_p`, `mirostat*`, and grammar fields. `n_predict` becomes
`max_tokens`, and `n_predict: 0`, the prompt warm-up call, becomes
`max_tokens: 1`: vLLM has no "process the prompt, generate nothing", and one
token is the cheapest answer. A negative `max_tokens` is dropped, which leaves
the limit to the engine.

It also drops `min_p` and `logit_bias` by default. This vLLM build rejects them
with HTTP 400 while speculative decoding is active. Set
`VLLM_KEEP_UNSUPPORTED=1` to pass them through.

`reasoning_format: "none"` becomes `chat_template_kwargs: {enable_thinking:
false}`. Streaming requests gain `stream_options: {include_usage: true}` and
`return_token_ids: true`, which is how the adapter learns exact token counts.

### Capability probing

Two things the UI cannot learn from a vLLM server on its own, so the adapter
finds out by asking the engine at startup. Both probes run before the port binds
and are logged:

```
vision support: yes (engine accepted an image)
thinking support: yes (engine emitted reasoning deltas)
```

Thinking. The UI decides whether a model can think by scanning the chat template
string it gets from `/props` (`src/lib/utils/chat-template-thinking-detector.ts:46`).
vLLM never serves its template, so that test cannot work. The adapter runs one
short streamed completion instead, and if any `delta.reasoning` arrives, `/props`
starts reporting thinking support. The control then reaches the UI, and choosing
"off" flows back through the `enable_thinking` mapping above.

Vision. The UI refuses image files unless `/props` says `modalities.vision`, and
the message is "Images require a vision-capable model"
(`src/lib/utils/modality-file-validation.ts:85`). Worse, `chat.service.ts:1077`
silently strips every `image_url` part from history when the flag is off, so an
upload can also disappear without a word. The adapter probes with one tiny
non-streaming request that carries a 1x1 PNG and `max_tokens: 1`: a 200 means the
engine accepts images, a rejection means it does not, and the reason is logged.
A request that carries an image also updates the flag at runtime, so a wrong
answer self-corrects on the next `/props` fetch. Set `VLLM_MODALITY_VISION=1` or
`=0` to skip the probe and decide yourself.

The image wire shape needs no translation: the UI sends
`{ type: "image_url", image_url: { url: "data:image/png;base64,..." } }`, which is
what vLLM expects, with no `detail` or llama.cpp-only extras. Once vision is on,
the `pdfAsImage` setting in the UI also becomes reachable, which renders PDF
pages as images and sends them the same way.

Audio and video stay off. The UI sends those as `input_audio` and `input_video`
parts, which are llama.cpp spellings vLLM does not accept.

### Cancellation

The UI stops a generation by closing the HTTP connection. The adapter watches
that event and aborts its own request to vLLM, which cancels the work on the
GPU. This matters: before this was wired up, closing the browser tab during a
long answer let vLLM run to 8092 tokens with nobody listening.

### Stream rewriting

Two changes happen to each streamed chunk:

1. `delta.reasoning` becomes `delta.reasoning_content`. llama.cpp names the
   reasoning field that way, and the UI only reads that name.
2. A `timings` object is added, in the shape llama.cpp uses.

The second part matters. The UI never measures time in the browser. It shows
tokens per second only from the `timings` object that the server attaches to
stream chunks (`src/lib/stores/chat/processing.svelte.ts:55`). Against a plain
OpenAI server that field is absent, so the live speed readout stays at zero and
the per-message statistics block disappears entirely. The adapter therefore
produces it:

- During generation, every chunk carries `predicted_n` and `predicted_ms`, from
  the running token count (`token_ids` in each chunk) and the clock since the
  first token. This drives the live tokens per second readout and the context
  gauge.
- The last chunk carries the full set: `prompt_n`, `prompt_ms`, `predicted_n`,
  `predicted_ms`, `cache_n`. This drives the per-message statistics, prompt
  processing speed, and generation speed.

### Where the final numbers come from

The adapter reads `GET /metrics` from vLLM before and after each request. It
diffs the Prometheus histograms, and trusts them only when the window is clean:
exactly one request finished during it, nothing else was in flight when it
started, and no counter moved backwards (an engine restart resets them). On a
busy shared server those conditions usually fail, and the adapter falls back to
the clock it measured itself. That costs some precision and keeps the number
true.

| Value | Engine source when attributable | Fallback |
| --- | --- | --- |
| `prompt_ms` | `vllm:request_prefill_time_seconds` | Time to first token, measured by the adapter |
| `predicted_ms` | `vllm:request_decode_time_seconds` | Clock between first and last chunk |
| `prompt_n` | `vllm:request_prefill_kv_computed_tokens` | `usage.prompt_tokens` |
| `cache_n` | `usage.prompt_tokens - prompt_n` | 0 |
| `predicted_n` | `usage.completion_tokens` | Counted `token_ids` |

`prompt_n` counts tokens the engine actually computed, and `cache_n` counts
tokens it reused from the prefix cache. That matches llama.cpp semantics, where
the UI adds the two, so the context gauge stays correct. Each request logs one
line to `var/adapter.log` with the source in the tag:

```
10:03:24 timings(engine) pp 63 tok 742.0 t/s | tg 900 tok 98.1 t/s | cache 0
```

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

The addition is a "vLLM engine" block at the bottom of the context gauge popup
(click the gauge dial next to the model badge while a chat is active). It shows
KV cache usage with a bar, live output and prompt throughput for the whole
engine, requests running and waiting, prefix cache hit rate, speculative
decoding accept rate and tokens per step, and average time to first token.
Files:

- `src/lib/services/vllm.service.ts` and `src/lib/stores/vllm.svelte.ts`
- `src/lib/components/app/chat/ChatForm/ChatFormContextGauge/VllmEngineStats.svelte`
- one `$effect` and one `{#if}` in `ContextGaugePopup.svelte`
- `ApiVllmStats` in `src/lib/types/api.d.ts`, plus export lines, plus one
  constant in `src/lib/constants/ui.constants.ts`

The block polls `/vllm/stats` every 3 seconds. A real llama.cpp server answers
404 on that path, so the block stays hidden and the poller stops.

## Failure modes

- If vLLM is unreachable, `/props` answers 503. The UI reads that as "backend not
  ready": it shows a spinner and retries once a second, so the page recovers on
  its own when vLLM comes back. Static files keep serving.
- If a completion fails upstream, the vLLM error body passes through with its own
  status, so the UI shows the real reason.
- If the metrics query fails, timings fall back to wall clock and the request
  still succeeds, because the chat path never depends on `/metrics` for an answer.

## Test it yourself

`e2e.mjs` drives the UI in headless Chromium: it sends a message, reads the live
tokens per second during streaming, opens the context gauge, dumps the engine
block, and reports any console, page, or HTTP error.

```sh
./run.sh -d
node e2e.mjs        # needs the chromium build that npx playwright install adds
```

Screenshots land in `shots/`. A passing run prints a live tokens-per-second
reading during streaming, per-message prompt and generation speed after it, the
engine rows from the popup, and `(none)` under problems.

`tests/vision-e2e.mjs` does the same for image input: it attaches a real PNG
through the UI file picker, sends it, and fails unless the request carried the
image and the answer repeats the text printed inside it. Point it at another
picture with `TEST_IMAGE=/path/to.png TEST_EXPECT=SOMEWORD`.

```sh
node tests/vision-e2e.mjs
```

## Test without a GPU

`tests/mock-vllm.mjs` is a small stand-in for vLLM: it reports a model, serves
`/metrics`, and streams a fixed answer. Point the adapter at it to exercise the
translation without any hardware. It rejects image input by default, so the
adapter's vision probe reports "no" against it, which is how to test the
capability path both ways: run the mock with `MOCK_VISION=1` and the probe
switches to "yes".

```sh
node tests/mock-vllm.mjs 8011 &
VLLM_UPSTREAM=http://127.0.0.1:8011 PORT=8012 node server/adapter.mjs
```

Send `"truncate": true` in a completion body to make the mock drop the stream
mid-answer, which is how to see the lost-stream path. The mock's metrics never
advance, so timings there come from the wall clock fallback: read the
`timings(wall)` tag in the log to tell the two sources apart.

## Security

The adapter has no authentication of its own, and it forwards
`VLLM_API_KEY` to the backend. It listens on all interfaces by default. Bind it
to a trusted network, put it behind your usual reverse proxy with access
control, or run it with `HOST=127.0.0.1`.

Responses sent to the browser carry no backend address: `/props` reports the
vLLM version only, and `/vllm/stats` reports engine numbers. The upstream URL and
any api key live in `.env`, which git ignores, and appear in the local boot log.

## Configuration

Set these in `.env`, or in the environment, which takes precedence. The adapter
reads `.env` from the project root at startup and prints the resolved backend in
its boot log.

| Variable | Default | Purpose |
| --- | --- | --- |
| `VLLM_UPSTREAM` | `http://localhost:8000` | vLLM base URL. |
| `PORT` / `HOST` | `8080` / `0.0.0.0` | Where the adapter listens. |
| `VLLM_MODEL` | first model reported | Model id to send when the UI omits it. |
| `VLLM_API_KEY` | empty | Sent as `Bearer` to vLLM if your server needs it. |
| `VLLM_N_CTX` | `max_model_len` | Context size shown by the gauge. |
| `VLLM_ENGINE_TIMINGS` | `1` | Set to `0` to use wall clock timings only. |
| `VLLM_MODALITY_VISION` | auto-detect | `1` forces image input on, `0` forces it off. Unset means probe the engine. |
| `VLLM_PROBE_VISION` | `1` | Set to `0` to skip the startup image probe and leave vision off. |
| `VLLM_PROBE_THINKING` | `1` | Set to `0` to skip the startup thinking probe. |
| `VLLM_KEEP_UNSUPPORTED` | `0` | Set to `1` to send `min_p` and `logit_bias` through instead of dropping them. |

## Known gaps

- No live prompt processing progress bar. llama.cpp sends `prompt_progress`
  while it evaluates the prompt. vLLM reports nothing per request until it
  finishes, so there is no measured number to show. The prompt speed appears when
  the answer completes.
- No stream resume. llama.cpp replays a dropped stream from a byte offset via
  `/v1/stream`. The adapter ends every normal stream with `[DONE]`, so that path
  never comes up. If the upstream connection dies mid-token, the adapter closes
  without `[DONE]` on purpose: the UI then reports "Stream connection lost", and
  keeps the partial answer, rather than showing a truncated answer as complete.
- No server-side tools. llama.cpp exposes `/tools` for file and shell access. The
  UI shows an empty list, and MCP tools configured in the browser still work.
- Chat template is unknown. vLLM does not serve it, so `/props` reports an empty
  string. The UI cannot auto-detect whether the model supports thinking, and the
  thinking toggle has no template to inspect. Reasoning still
  streams and displays, because it arrives in `delta.reasoning_content`.
- Multimodal covers images only. Audio and video are reported off, because the
  UI sends those in llama.cpp-only part types that vLLM rejects.

## Layout

```
server/adapter.mjs   the whole backend, Node stdlib, no dependencies
frontend/            extracted llama.cpp webui source plus the vLLM additions
dist/                built frontend, what the adapter serves
build.sh             rebuild dist/ from frontend/, optionally re-extract first
run.sh               start, stop, and status for the adapter
e2e.mjs              headless browser test of the running UI
tests/mock-vllm.mjs  fake vLLM server, for testing without a GPU
tests/vision-e2e.mjs browser test that an attached image reaches the model
patches/             the frontend change set, plus the script that makes it
.env                 your backend host and key, never committed
.env.example         template for it
var/                 background log and pid file
shots/               screenshots written by the browser tests
```

## Provenance

`frontend/` is a copy of the llama.cpp webui, taken from `tools/ui` in
ggml-org/llama.cpp at commit `d7a695ef679138c13d86359b84c1731d36213d32`. That code
is MIT licensed, the license text is in `LICENSE.llama.cpp`, and `frontend/README.md`
is the upstream document, describing the llama.cpp build that this project does not
use. Everything under `server/`, `tests/`, and `patches/` was written for this
port.

To move to a newer upstream, run `./build.sh --from <path>/tools/ui`. It extracts,
re-applies the vLLM patch, and rebuilds. If the patch no longer applies, the
command names the files upstream moved.

Requirements: Node 18 or newer (tested on Node 24) and a reachable vLLM server.
