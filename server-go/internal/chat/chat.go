package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llama-webui/server/internal/backend"
)

// maxBodyBytes caps what the proxy will read from a browser: a chat request
// carries the whole conversation, so it is generous.
const maxBodyBytes = 64 * 1024 * 1024

// postWindowDeadline bounds the second metrics read, so a hung /metrics cannot
// delay the tail of an answer.
const postWindowDeadline = 1500 * time.Millisecond

// Proxy serves POST /v1/chat/completions for one backend.
type Proxy struct {
	b backend.Backend
	d *backend.Deps
}

// New wires a proxy to a backend.
func New(b backend.Backend, d *backend.Deps) *Proxy { return &Proxy{b: b, d: d} }

// Translate applies the backend request rewrite. It is exported so the router
// can report what it sent.
func (p *Proxy) Translate(body map[string]any) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	p.b.Translate(out)
	p.b.Prepare(out)
	return out
}

// Complete handles one completion request, streamed or not.
func (p *Proxy) Complete(w http.ResponseWriter, r *http.Request) {
	d, b := p.d, p.b
	body, err := readBody(r)
	if err != nil {
		WriteMessage(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	out := p.Translate(body)
	if backend.Str(out, "model") == "" {
		out["model"] = d.Models.DefaultID(r.Context(), d.Cfg.Model)
	}

	// vLLM reports nothing in the stream, so a metrics window around each request
	// is the only way to get its real numbers. Start the first scrape before the
	// request leaves, but do not wait for it: the engine prefill covers the round
	// trip. Strata puts timings in the stream, so it needs no window.
	needWindow := b.WindowsTimings() && d.Cfg.EngineTimings
	win := newWindow()
	if needWindow {
		win.start(r.Context(), b, d)
	}
	defer win.wait()
	tSend := time.Now()

	// The UI cancels a generation by closing the fetch, so the only reliable
	// signal is this response going away before it was finished. Without the
	// abort, the engine keeps generating into a dead socket.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	upReq, err := d.Up.NewRequest(ctx, http.MethodPost, "/v1/chat/completions", marshalBody(out))
	if err != nil {
		WriteMessage(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstream, err := d.Up.Client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			d.Log("client aborted before the engine answered")
			return
		}
		WriteMessage(w, http.StatusBadGateway,
			fmt.Sprintf("cannot reach the %s backend at %s: %s", b.ID(), d.Cfg.Upstream, err))
		return
	}
	defer upstream.Body.Close()

	// keep the advertised capability in step with what the engine really does
	sentImage := hasImagePart(body["messages"])

	if !statusOK(upstream.StatusCode) {
		text, _ := io.ReadAll(io.LimitReader(upstream.Body, 1<<20))
		if sentImage && d.Flags.Vision.Get() && backend.ImageRejectRe.Match(text) {
			d.Flags.Vision.Set(false)
			d.Log("vision support: off, the engine rejected an image request")
		}
		ct := upstream.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(upstream.StatusCode)
		_, _ = w.Write(text)
		return
	}

	if sentImage && !d.Flags.Vision.Get() {
		d.Flags.Vision.Set(true)
		d.Log("vision support: on, the engine accepted an image request")
	}

	if !backend.Bool(out, "stream") {
		payload, err := backend.DecodeJSONStream(io.LimitReader(upstream.Body, maxBodyBytes))
		if err != nil {
			d.Log("upstream returned unreadable JSON: %s", err)
			WriteMessage(w, http.StatusBadGateway, fmt.Sprintf("the %s backend returned unreadable JSON", b.ID()))
			return
		}
		WriteJSON(w, http.StatusOK, mapMessageReasoning(payload))
		return
	}

	p.stream(ctx, w, upstream, out, needWindow, win, tSend)
}

// window is the opening half of the metrics window around one request.
type window struct {
	done chan struct{}
	pre  *backend.Snapshot
}

func newWindow() *window {
	w := &window{done: make(chan struct{})}
	close(w.done)
	return w
}

// start takes the first scrape, before the request leaves.
func (w *window) start(ctx context.Context, b backend.Backend, d *backend.Deps) {
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		w.pre = backend.SafeSnapshot(ctx, b, d, backend.SnapshotOpts{Force: true})
	}()
}

// ready blocks until the opening scrape is done and returns it.
func (w *window) preSnapshot() *backend.Snapshot {
	w.wait()
	return w.pre
}

// wait keeps the scrape goroutine accounted for: it is bounded by the fetch
// deadline, and by the caller's context once the handler is over.
func (w *window) wait() { <-w.done }

func (p *Proxy) stream(ctx context.Context, w http.ResponseWriter, upstream *http.Response, out map[string]any, needWindow bool, win *window, tSend time.Time) {
	d, b := p.d, p.b
	st := &backend.Stream{}
	var pending map[string]any // the usage chunk, held for the final timings
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	// Go blocks a Write until the client has taken the bytes, which is the
	// backpressure the Node version reaches by waiting for the drain event: a
	// slow reader never makes us hold the rest of the answer in memory.
	event := func(v any) error {
		body, err := marshalData(v)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		return rc.Flush()
	}

	handle := func(line string) error {
		if !strings.HasPrefix(line, "data:") {
			return nil // blank separators and comment lines
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			st.SawDone = true
			return nil
		}
		chunk, err := backend.DecodeJSON(payload)
		if err != nil {
			return nil // a chunk we cannot read is not worth failing the answer over
		}
		if id := backend.Str(chunk, "id"); id != "" {
			st.ID = id
		}
		if m := backend.Str(chunk, "model"); m != "" {
			st.Model = m
		}
		// an engine that measures its own speed is always the better source
		if t := backend.Obj(chunk, "timings"); t != nil {
			st.Inband = t
		}
		if raw, ok := chunk["error"]; ok && backend.Truthy(raw) {
			msg := backend.Str(backend.Obj(chunk, "error"), "message")
			if msg == "" {
				msg = "the engine reported an error"
			}
			st.Err = msg
			d.Log("engine error frame: %s", msg)
			return nil
		}
		b.RewriteChunk(chunk, st)

		if usage := backend.Obj(chunk, "usage"); usage != nil {
			// held back until the post-request snapshot, then sent last
			st.Usage = usage
			pending = chunk
			return nil
		}

		// live decode speed: the UI only knows t/s from server timings
		if st.Tokens >= 2 && !st.FirstAt.IsZero() {
			if ms := time.Since(st.FirstAt).Milliseconds(); ms > 0 {
				chunk["timings"] = map[string]any{
					"predicted_n":  st.Tokens,
					"predicted_ms": backend.Round1(float64(ms)),
				}
			}
		}
		return event(chunk)
	}

	aborted := false
	br := bufio.NewReaderSize(upstream.Body, 16*1024)
	for {
		if ctx.Err() != nil {
			aborted = true
			break
		}
		// A line may arrive without its newline: keep the tail and flush it when
		// the stream ends, or the last chunk is lost.
		line, err := br.ReadString('\n')
		if line != "" {
			if werr := handle(strings.TrimRight(line, "\r\n")); werr != nil {
				aborted = true
				if ctx.Err() == nil {
					d.Log("client write failed: %s", werr)
				}
				break
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() == nil {
				st.Truncated = true
				d.Log("upstream stream error: %s", err)
			} else {
				aborted = true
			}
			break
		}
	}

	pre := win.preSnapshot()
	if ctx.Err() != nil {
		aborted = true
	}
	if aborted {
		d.Log("client aborted after %d tokens, engine cancelled", st.Tokens)
		return
	}
	st.Truncated = st.Truncated || !st.SawDone || st.Err != ""

	var post *backend.Snapshot
	if needWindow {
		post = backend.SafeSnapshot(ctx, b, d, backend.SnapshotOpts{Force: true, Timeout: postWindowDeadline})
	}
	timings := b.FinalTimings(st, pre, post, tSend)

	if st.Truncated {
		// no [DONE] here: the UI reads that as a finished answer, and a cut-off
		// reply must not look complete
		d.Log("upstream ended early after %d tokens, reporting a lost stream", st.Tokens)
		return
	}

	if pending != nil {
		pending["timings"] = timings
	} else {
		pending = map[string]any{
			"id":      orText(st.ID, "adapter"),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   orText(st.Model, backend.Str(out, "model")),
			"choices": []any{},
			"timings": timings,
		}
	}
	if err := event(pending); err != nil {
		d.Log("client write failed: %s", err)
		return
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	_ = rc.Flush()
}

// mapMessageReasoning renames the thinking field of a non-streamed reply.
func mapMessageReasoning(payload map[string]any) map[string]any {
	msg := backend.Obj(backend.FirstChoice(payload), "message")
	if r := backend.Str(msg, "reasoning"); r != "" {
		msg["reasoning_content"] = r
		delete(msg, "reasoning")
	}
	return payload
}

// hasImagePart reports whether the UI sent an image in this conversation.
func hasImagePart(v any) bool {
	msgs, ok := v.([]any)
	if !ok {
		return false
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := mm["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			pm, ok := part.(map[string]any)
			if ok && backend.Str(pm, "type") == "image_url" {
				return true
			}
		}
	}
	return false
}

func readBody(r *http.Request) (map[string]any, error) {
	// An empty body is an empty request, the way the Node version reads it.
	if r.Body == nil {
		return map[string]any{}, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("request body over %s bytes", strconv.Itoa(maxBodyBytes))
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, nil
	}
	return backend.DecodeJSON(string(body))
}

func marshalBody(out map[string]any) []byte {
	body, err := json.Marshal(out)
	if err != nil {
		return []byte("{}")
	}
	return body
}

func marshalData(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), body...), '\n', '\n'), nil
}

func statusOK(code int) bool { return code >= 200 && code <= 299 }

func orText(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
