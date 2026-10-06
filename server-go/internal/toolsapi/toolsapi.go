// Package toolsapi serves the tool endpoints the llama.cpp webui calls: GET and
// POST /tools, plus the CORS proxy the browser uses to reach remote MCP servers.
// Response shapes follow tools/server/server-tools.cpp, and the proxy follows
// tools/server/server-cors-proxy.h.
package toolsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"sort"
	"sync"

	"llama-webui/server/internal/contracts"
)

// Log is the logger the caller hands to New and NewProxy.
type Log func(format string, v ...any)

const (
	jsonType = "application/json; charset=utf-8"
	sseType  = "text/event-stream"

	// execShellCommand is the only tool llama-server can stream. It is the
	// fallback rule for a registry that does not declare its own capability.
	execShellCommand = "exec_shell_command"

	// runtimeMessage answers a call that asks for an isolate (docker, podman or
	// ssh). llama-server attaches to one. This server only runs on the host, and
	// answering is safer than running somewhere the operator did not ask for.
	runtimeMessage = "tool runtime is not supported by this server"

	// Headers a model may not set by hand. Each key is deleted from params
	// first. The cwd and resp_type headers take their place. A runtime header is
	// refused outright.
	hdrToolCwd     = "x-tool-cwd"
	hdrToolRuntime = "x-tool-runtime"
	hdrRespType    = "x-resp-type"

	// maxCallBodyBytes bounds one POST /tools body. A base64 read_file result
	// carried inside a write_file param still fits comfortably.
	maxCallBodyBytes = 16 << 20
)

const (
	kindInvalidRequest  = "invalid_request_error"
	kindAuthentication  = "authentication_error"
	kindServer          = "server_error"
	kindFeatureDisabled = "feature_disabled"
)

// streamer is the optional registry capability. A registry that implements it
// decides for itself which tools may stream (builtin.Set does).
type streamer interface {
	SupportsStream(name string) bool
}

// API serves /tools over one registry of tools.
type API struct {
	reg   contracts.Registry
	log   Log
	allow func(*http.Request) bool
}

// New returns nil when reg holds no tools: the caller then answers 403 the way
// llama-server does for a server started without --tools.
func New(reg contracts.Registry, logf Log) *API {
	if reg == nil || len(reg.List()) == 0 {
		return nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &API{reg: reg, log: logf}
}

// Tools is the contracts.Registry the API exposes, so a caller can also ask it
// questions directly. It returns nil for a nil API.
func (a *API) Tools() contracts.Registry {
	if a == nil {
		return nil
	}
	return a.reg
}

// Keys gates both handlers: allow reports whether a request may call a tool.
// llama-server reads --api-key in a routing middleware, so the caller keeps
// the key parsing and passes the verdict down here. Setting it to nil (or never
// calling Keys) leaves the endpoints open, like a server with no --api-key.
//
// Set it before the server starts: it is read without a lock.
func (a *API) Keys(allow func(*http.Request) bool) *API {
	if a == nil {
		return nil
	}
	a.allow = allow
	return a
}

// HandleList serves GET /tools.
func (a *API) HandleList(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeDisabled(w)
		return
	}
	if !a.authorized(w, r) {
		return
	}
	// llama-server lists tools in build_tools() order. Sorting by name keeps the
	// UI list stable when a MCP server re-registers its tools. The copy leaves the
	// order inside the registry alone.
	listed := a.reg.List()
	infos := make([]contracts.ToolInfo, len(listed))
	copy(infos, listed)
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Tool < infos[j].Tool })
	writeBody(w, http.StatusOK, jsonType, encodeJSON(infos))
}

// HandleCall serves POST /tools.
func (a *API) HandleCall(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeDisabled(w)
		return
	}
	if !a.authorized(w, r) {
		return
	}
	req, runtime, ok := a.readCall(w, r)
	if !ok {
		return
	}
	tool, found := a.reg.Get(req.Name)
	if !found {
		writeFlat(w, http.StatusNotFound, http.StatusBadRequest,
			fmt.Sprintf("unknown tool %q", req.Name), kindInvalidRequest)
		return
	}
	if req.Stream && !a.canStream(req.Name) {
		writeFlat(w, http.StatusNotFound, http.StatusBadRequest,
			fmt.Sprintf("tool %q does not support stream = true", req.Name), kindInvalidRequest)
		return
	}
	// llama-server resolves the runtime inside the tool call, after find_tool, so
	// a bad tool name is reported first.
	if runtime != "" {
		a.log("rejected a tool runtime request for tool %s", req.Name)
		writeFlat(w, http.StatusInternalServerError, http.StatusInternalServerError,
			runtimeMessage, kindServer)
		return
	}
	if req.Stream {
		a.serveStream(w, r, tool, req)
		return
	}
	a.serveOnce(w, r, tool, req)
}

// authorized answers 401 the way the API key middleware in server-http.cpp
// does, and reports whether the handler may go on.
func (a *API) authorized(w http.ResponseWriter, r *http.Request) bool {
	if a.allow == nil || a.allow(r) {
		return true
	}
	a.log("unauthorized: Invalid API Key")
	writeWrapped(w, http.StatusUnauthorized, "Invalid API Key", kindAuthentication, http.StatusUnauthorized)
	return false
}

// canStream uses the capability the registry declares. A registry that stays
// silent is treated like llama-server, where only exec_shell_command streams.
func (a *API) canStream(name string) bool {
	if s, ok := a.reg.(streamer); ok {
		return s.SupportsStream(name)
	}
	return name == execShellCommand
}

// readCall decodes the body and folds the tool headers into the request. It
// returns the x-tool-runtime header as the second value: a runtime is refused
// later, once the tool has been looked up, as llama-server resolves it inside
// the tool call. It writes the error response and returns false when the body
// is not usable.
func (a *API) readCall(w http.ResponseWriter, r *http.Request) (contracts.ToolRequest, string, bool) {
	var req contracts.ToolRequest

	// MaxBytesReader stops the read at the cap instead of letting a client make
	// this server allocate a body it will never use. The failure takes the same
	// path as any other unreadable body.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallBodyBytes))
	if err != nil {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error(), kindInvalidRequest)
		return req, "", false
	}

	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
			"invalid JSON in request body: "+err.Error(), kindInvalidRequest)
		return req, "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
			"request body must be a JSON object, but is "+typeName(raw), kindInvalidRequest)
		return req, "", false
	}

	// body.at("tool") throws out_of_range when the key is missing, and
	// get<std::string>() throws type_error when it holds anything else, null
	// included.
	v, present := obj["tool"]
	if !present {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest, "key 'tool' not found", kindInvalidRequest)
		return req, "", false
	}
	name, isStr := v.(string)
	if !isStr {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
			"type must be string, but is "+typeName(v), kindInvalidRequest)
		return req, "", false
	}

	params, isObj := objectField(obj, "params")
	if !isObj {
		writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
			"type must be object, but is "+typeName(obj["params"]), kindInvalidRequest)
		return req, "", false
	}

	stream := false
	if v, present := obj["stream"]; present {
		b, isBool := v.(bool)
		if !isBool {
			writeFlat(w, http.StatusBadRequest, http.StatusBadRequest,
				"type must be boolean, but is "+typeName(v), kindInvalidRequest)
			return req, "", false
		}
		stream = b
	}

	// Headers win over params: llama-server deletes each key first, so a model
	// cannot set it by hand and only the caller holding the connection can.
	delete(params, "cwd")
	delete(params, "resp_type")
	// A runtime the model put in params is dropped, as it is in llama-server.
	// Only the header can ask for an isolate, and none is reachable here.
	delete(params, "runtime")

	req.Name = name
	req.Params = params
	req.Cwd = r.Header.Get(hdrToolCwd)
	req.RespType = r.Header.Get(hdrRespType)
	req.Stream = stream
	return req, r.Header.Get(hdrToolRuntime), true
}

// serveOnce runs a tool and answers with its result. A tool that fails reports
// through Result.Error, and llama-server still answers 200 with that body.
func (a *API) serveOnce(w http.ResponseWriter, r *http.Request, tool contracts.Tool, req contracts.ToolRequest) {
	res, ok := a.invoke(r.Context(), tool, req, nil)
	if !ok {
		writeFlat(w, http.StatusInternalServerError, http.StatusInternalServerError,
			fmt.Sprintf("tool %q panicked", req.Name), kindServer)
		return
	}
	writeBody(w, http.StatusOK, jsonType, res.JSON())
}

// invoke runs one tool call. A panic is logged with the tool name and reported
// as ok = false, so it cannot take down the request or the process.
func (a *API) invoke(ctx context.Context, tool contracts.Tool, req contracts.ToolRequest, out contracts.Sink) (res contracts.Result, ok bool) {
	ok = true
	defer func() {
		if rec := recover(); rec != nil {
			a.log("tool %s panicked: %v\n%s", req.Name, rec, debug.Stack())
			res, ok = contracts.Result{}, false
		}
	}()
	return tool.Invoke(ctx, req, out), true
}

// serveStream writes the result frames. Everything runs in the handler
// goroutine: the tool pushes into s.write, so no goroutine outlives the
// request. Go cancels r.Context() when the connection closes (the deprecated
// CloseNotifier is exactly this behaviour), and the cancel below stops the tool
// as soon as a frame cannot be written.
func (a *API) serveStream(w http.ResponseWriter, r *http.Request, tool contracts.Tool, req contracts.ToolRequest) {
	h := w.Header()
	h.Set("Content-Type", sseType)
	h.Set("Cache-Control", "no-cache")
	// same hint llama-server gives a reverse proxy in front of a stream
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s := &streamWriter{w: w, cancel: cancel}
	res, ok := a.invoke(ctx, tool, req, s.write)
	switch {
	case !ok:
		s.done(fmt.Sprintf("tool %q panicked", req.Name))
	case res.Error != "":
		s.done(res.Error)
	default:
		s.done("")
	}
}

// streamWriter turns the Sink of a tool into SSE frames.
type streamWriter struct {
	w      io.Writer
	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
}

// write emits one chunk frame. Empty chunks are dropped, as llama-server does,
// so a tool that flushes on every byte does not push filler frames.
func (s *streamWriter) write(chunk string) {
	if chunk == "" {
		return
	}
	s.frame(streamFrame{Chunk: chunk})
}

// done emits the terminal frame. There is no [DONE] sentinel on this route.
func (s *streamWriter) done(err string) {
	s.frame(streamFrame{Done: true, Error: err})
}

func (s *streamWriter) frame(f streamFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// The mutex is held, so a tool that pushes from its own goroutine cannot
	// interleave a chunk into the middle of the terminal frame.
	s.writeFrame(f)
}

func (s *streamWriter) writeFrame(f streamFrame) {
	out := append([]byte("data: "), encodeJSON(f)...)
	out = append(out, '\n', '\n')
	if _, err := s.w.Write(out); err != nil {
		s.closed = true
		s.cancel()
		return
	}
	if fl, ok := s.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// streamFrame is one SSE payload: server_tool_stream_result::to_json.
type streamFrame struct {
	Chunk string `json:"chunk,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

func flush(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// objectField reads an optional object member, defaulting to an empty one.
func objectField(obj map[string]any, key string) (map[string]any, bool) {
	v, present := obj[key]
	if !present || v == nil {
		return map[string]any{}, true
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

// typeName names a decoded JSON value the way llama-server error texts do.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// flatError is the body server-tools.cpp writes for a request error on /tools.
type flatError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

// wrappedError is the body llama-server writes for a rejected API key and for a
// disabled route: the same fields inside an "error" object.
type wrappedError struct {
	Error wrappedFields `json:"error"`
}

type wrappedFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code,omitempty"`
}

func writeFlat(w http.ResponseWriter, status, code int, message, kind string) {
	writeBody(w, status, jsonType, encodeJSON(flatError{Code: code, Message: message, Type: kind}))
}

func writeWrapped(w http.ResponseWriter, status int, message, kind string, code int) {
	writeBody(w, status, jsonType, encodeJSON(wrappedError{
		Error: wrappedFields{Message: message, Type: kind, Code: code},
	}))
}

// writeDisabled is the answer for a route llama-server did not register: /tools
// without --tools, /cors-proxy without --ui-mcp-proxy.
func writeDisabled(w http.ResponseWriter) {
	writeWrapped(w, http.StatusForbidden, "this feature is disabled", kindFeatureDisabled, 0)
}

func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// encodeJSON renders a value the way nlohmann does: no HTML escaping and no
// trailing newline.
func encodeJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"code":500,"message":"serialization failed","type":"server_error"}`)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
