package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"llama-webui/server/internal/contracts"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Values copied from server-mcp.cpp. They are variables so the tests can
// shorten them.
var (
	cooldown    = 5 * time.Second  // MCP_COOLDOWN_SECONDS
	warmupLimit = 10 * time.Second // MCP_WARMUP_TIMEOUT_SECONDS
)

// clientInfo is what this client sends in the initialize handshake. The C++
// client sends {"name":"llama.cpp","version":"1.0"}.
var clientInfo = &mcp.Implementation{Name: "llama-webui", Version: "1.0"}

// Options tunes a Client. The zero value is fine.
type Options struct {
	// Logger takes the warmup, collision and transport messages. A nil logger
	// drops them.
	Logger *slog.Logger
	// ReservedNames seeds the collision set. Pass the names of the built-in
	// tools here, so an MCP tool never takes one of them.
	ReservedNames []string
}

// Client holds the configured MCP servers and the tools discovered on them.
// It implements contracts.Registry.
type Client struct {
	cfg    Config
	log    *slog.Logger
	sdk    *mcp.Client
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.RWMutex
	tools    map[string]*tool
	order    []string
	reserved map[string]bool

	ownMu sync.Mutex
	own   map[*mcp.ClientSession]*entry

	entries  map[string]*entry // by server name, built once in New
	stopping atomic.Bool
}

// New parses nothing: it takes a Config from Load, discovers the tools of every
// server, and returns a client that is ready to serve calls.
//
// Discovery runs one server at a time. Each server is started, listed, and
// closed again, with a cap of 10 seconds per server. A server that will not
// start is logged and left with no tools; calls to it report it as unavailable
// later. Cancelling ctx shuts the client down, like Shutdown.
func New(ctx context.Context, cfg Config, opts *Options) (*Client, error) {
	if opts == nil {
		opts = &Options{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &Client{
		cfg:      cfg,
		log:      log,
		ctx:      cctx,
		cancel:   cancel,
		tools:    make(map[string]*tool),
		reserved: make(map[string]bool, len(opts.ReservedNames)),
		own:      make(map[*mcp.ClientSession]*entry),
		entries:  make(map[string]*entry, len(cfg.Servers)),
	}
	for _, name := range opts.ReservedNames {
		c.reserved[name] = true
	}
	c.sdk = mcp.NewClient(clientInfo, &mcp.ClientOptions{
		Logger:                 log,
		ToolListChangedHandler: func(_ context.Context, req *mcp.ToolListChangedRequest) { c.onListChanged(req) },
	})

	for _, sc := range cfg.Servers {
		if _, dup := c.entries[sc.Name]; dup {
			cancel()
			return nil, fmt.Errorf(errDuplicateServer, sc.Name)
		}
		if sc.URL != "" {
			u, err := url.Parse(sc.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				cancel()
				return nil, fmt.Errorf("MCP server %q: url %q is not an http or https endpoint", sc.Name, sc.URL)
			}
		}
		c.entries[sc.Name] = &entry{c: c, cfg: sc}
	}

	c.warmup()
	return c, nil
}

// Registry is the interface the HTTP layer wants.
func (c *Client) Registry() contracts.Registry { return c }

// List returns the tool infos in discovery order.
func (c *Client) List() []contracts.ToolInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]contracts.ToolInfo, 0, len(c.order))
	for _, name := range c.order {
		out = append(out, c.tools[name].info)
	}
	return out
}

// Get looks a namespaced tool name up.
func (c *Client) Get(name string) (contracts.Tool, bool) {
	c.mu.RLock()
	t, ok := c.tools[name]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return t, true
}

// Shutdown stops accepting work, aborts the calls in flight, and kills every
// child process. It waits for the teardown, and is safe to call twice.
func (c *Client) Shutdown() {
	c.stopping.Store(true)
	c.cancel()
	for _, name := range c.serverNames() {
		c.entries[name].close()
	}
}

func (c *Client) serverNames() []string {
	names := make([]string, 0, len(c.entries))
	for name := range c.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// warmup starts each server once, lists it, and closes it again.
func (c *Client) warmup() {
	for _, sc := range c.cfg.Servers {
		if c.ctx.Err() != nil {
			break
		}
		defs, err := c.entries[sc.Name].discover()
		if err != nil {
			c.log.Warn(fmt.Sprintf(msgWarmupSpawn, sc.Name, err))
			continue
		}
		c.log.Info(fmt.Sprintf(msgWarmup, sc.Name, len(defs)))
		for _, d := range defs {
			c.register(d)
		}
	}
}

func (c *Client) register(d toolDef) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registerLocked(d)
}

func (c *Client) registerLocked(d toolDef) bool {
	name := d.namespaced()
	if c.reserved[name] || c.tools[name] != nil {
		c.log.Warn(fmt.Sprintf(msgCollision, name, d.serverName))
		return false
	}
	c.tools[name] = newTool(c.entries[d.serverName], d)
	c.order = append(c.order, name)
	return true
}

// replace swaps in a fresh tool list for one server, after a
// notifications/tools/list_changed. Tools of other servers keep their place.
func (c *Client) replace(server string, defs []toolDef) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	order := make([]string, 0, len(c.order)+len(defs))
	for _, name := range c.order {
		if c.tools[name].def.serverName == server {
			delete(c.tools, name)
			continue
		}
		order = append(order, name)
	}
	c.order = order
	added := 0
	for _, d := range defs {
		if c.registerLocked(d) {
			added++
		}
	}
	return added
}

// onListChanged runs on the SDK read loop, so the work moves to a goroutine:
// a fresh tools/list is a request, and its reply needs that loop.
func (c *Client) onListChanged(req *mcp.ToolListChangedRequest) {
	sess, ok := req.GetSession().(*mcp.ClientSession)
	if !ok {
		return
	}
	if e := c.owner(sess); e != nil {
		go e.refresh()
	}
}

func (c *Client) owner(sess *mcp.ClientSession) *entry {
	c.ownMu.Lock()
	defer c.ownMu.Unlock()
	return c.own[sess]
}

// callCtx bounds one request: timeout_ms for the call and for the spawn it may
// need, and the shutdown flag on top of the caller context.
func (c *Client) callCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	out, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(c.ctx, func() { cancel() })
	return out, func() { stop(); cancel() }
}

// entry is one server, and the cache of its transport. It is the Go form of
// server_mcp_transport plus the parts of server_mcp that own it.
type entry struct {
	c   *Client
	cfg ServerConfig

	mu    sync.Mutex // one call at a time, like rpc_mutex
	sess  *mcp.ClientSession
	child *child // nil for an http server
	alive bool
	dead  time.Time // end of the spawn-failure cooldown
	last  string    // last error, for diagnostics
}

// discover is the startup path: start, list, close. The cap is the warmup
// limit, not timeout_ms, so a slow server does not stall startup for the full
// per-call budget.
func (e *entry) discover() ([]toolDef, error) {
	ctx, cancel := context.WithTimeout(e.c.ctx, warmupLimit)
	defer cancel()
	sess, ch, err := e.c.connect(ctx, e.cfg)
	e.child = ch // warmup runs before the client is shared, so no lock is needed
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	tools, err := listTools(ctx, sess)
	if err != nil {
		return nil, err
	}
	return defsFor(tools, e.cfg.Name), nil
}

// call is server_mcp::call_tool plus server_mcp_transport::call_tool.
func (e *entry) call(ctx context.Context, name string, args map[string]any) contracts.Result {
	if e.c.stopping.Load() {
		return contracts.Result{Error: textUnavailable + e.cfg.Name}
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	cctx, cancel := e.c.callCtx(ctx, time.Duration(e.cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	// The budget is already gone, so do not fork a child that cannot answer.
	if cctx.Err() != nil {
		return contracts.Result{Error: classify(cctx.Err(), cctx, e.c.stopping.Load())}
	}

	sess, err := e.sessionLocked(cctx)
	if err != nil {
		return contracts.Result{Error: textUnavailable + e.cfg.Name}
	}
	res, callErr := sess.CallTool(cctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if callErr != nil {
		e.last = callErr.Error()
		if classify(callErr, cctx, e.c.stopping.Load()) == textClosed {
			// The pipe is gone. Drop the session now, so the next call respawns
			// instead of failing on the same dead transport.
			e.c.log.Warn(fmt.Sprintf(msgNotAlive, e.cfg.Name, e.diagnosticsLocked()))
			e.closeLocked()
		}
	}
	r := replyFromCall(res, callErr)
	return r.convert(e.c.log, e.cfg.Name+"_"+name, cctx, e.c.stopping.Load())
}

// refresh re-lists a live server. It never spawns one: a notification only
// arrives on a session that is already open.
func (e *entry) refresh() {
	e.mu.Lock()
	defer e.mu.Unlock()
	sess := e.sess
	if sess == nil || !e.alive {
		return
	}
	ctx, cancel := e.c.callCtx(e.c.ctx, time.Duration(e.cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	tools, err := listTools(ctx, sess)
	if err != nil {
		e.last = err.Error()
		e.c.log.Warn(fmt.Sprintf(msgListFailed, e.cfg.Name, err))
		return
	}
	defs := defsFor(tools, e.cfg.Name)
	e.c.replace(e.cfg.Name, defs)
	e.c.log.Info(fmt.Sprintf(msgRefreshed, e.cfg.Name, len(defs)))
}

// close drops the cached transport. The child dies with it.
func (e *entry) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeLocked()
}

func (e *entry) closeLocked() {
	sess := e.sess
	if sess == nil {
		return
	}
	e.sess, e.alive = nil, false
	e.c.ownMu.Lock()
	delete(e.c.own, sess)
	e.c.ownMu.Unlock()
	if err := sess.Close(); err != nil && e.last == "" {
		e.last = err.Error()
	}
}

// sessionLocked returns a live session, spawning one when needed. A spawn
// failure parks the server in the cooldown, exactly like get_or_create.
func (e *entry) sessionLocked(ctx context.Context) (*mcp.ClientSession, error) {
	if e.c.stopping.Load() || ctx.Err() != nil {
		return nil, errStopped
	}
	if !e.dead.IsZero() {
		if now := time.Now(); now.Before(e.dead) {
			return nil, fmt.Errorf("in cooldown for %v", e.dead.Sub(now).Round(time.Millisecond))
		}
		e.dead = time.Time{}
	}
	if e.sess != nil {
		if e.alive {
			return e.sess, nil
		}
		e.c.log.Warn(fmt.Sprintf(msgNotAlive, e.cfg.Name, e.diagnosticsLocked()))
		e.closeLocked()
	}
	sess, ch, err := e.c.connect(ctx, e.cfg)
	if err != nil {
		e.child = ch
		e.dead = time.Now().Add(cooldown)
		e.last = err.Error()
		e.c.log.Warn(fmt.Sprintf(msgFailedStart, e.cfg.Name, e.diagnosticsLocked()))
		return nil, err
	}
	e.sess, e.child, e.alive = sess, ch, true
	e.c.ownMu.Lock()
	e.c.own[sess] = e
	e.c.ownMu.Unlock()
	go func(sess *mcp.ClientSession) {
		err := sess.Wait()
		e.mu.Lock()
		if e.sess == sess {
			e.alive = false
			if err != nil && e.last == "" {
				e.last = err.Error()
			}
		}
		e.mu.Unlock()
	}(sess)
	return sess, nil
}

// diagnosticsLocked is server_mcp_stdio::diagnostics: the last error plus the
// tail of the child stderr.
func (e *entry) diagnosticsLocked() string {
	out := e.last
	var s string
	if e.child != nil {
		s = e.child.tail.String()
	}
	if s != "" {
		if out != "" {
			out += "; "
		}
		out += "last stderr: " + s
	}
	return out
}

// errStopped marks a server that will not start because the client is going
// away. The caller turns it into the unavailable text.
var errStopped = fmt.Errorf("client is shutting down")

func listTools(ctx context.Context, sess *mcp.ClientSession) ([]*mcp.Tool, error) {
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

func defsFor(tools []*mcp.Tool, server string) []toolDef {
	out := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		if t == nil || t.Name == "" {
			continue
		}
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			raw = nil
		}
		out = append(out, toolDef{
			serverName:  server,
			name:        t.Name,
			description: t.Description,
			inputSchema: raw,
		})
	}
	return out
}

// toolDef is server_mcp_tool_def: what one discovery pass learned about a tool.
type toolDef struct {
	serverName  string
	name        string // bare name, no "<server>_" prefix
	description string
	inputSchema json.RawMessage
}

func (d toolDef) namespaced() string { return d.serverName + "_" + d.name }

// functionSpec is the "function" object of an OpenAI tool definition.
type functionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// definition is the OpenAI function object that server_mcp_tool::get_definition
// builds. A schema that is missing, null, or not an object becomes {}.
func (d toolDef) definition() json.RawMessage {
	out, err := json.Marshal(struct {
		Type     string       `json:"type"`
		Function functionSpec `json:"function"`
	}{
		Type:     "function",
		Function: functionSpec{Name: d.namespaced(), Description: d.description, Parameters: d.parameters()},
	})
	if err != nil {
		return json.RawMessage(`{"type":"function"}`)
	}
	return out
}

var emptyObject = json.RawMessage(`{}`)

func (d toolDef) parameters() json.RawMessage {
	if !isJSONObject(d.inputSchema) {
		return emptyObject
	}
	return d.inputSchema
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return false
	}
	return true
}

// tool is one discovered MCP tool. It implements contracts.Tool.
type tool struct {
	e    *entry
	def  toolDef
	info contracts.ToolInfo
}

func newTool(e *entry, d toolDef) *tool {
	t := &tool{e: e, def: d, info: contracts.ToolInfo{
		DisplayName: d.namespaced(), // server_mcp_tool sets display_name to the prefixed name
		Tool:        d.namespaced(),
		Type:        "mcp",
		UsesCwd:     false,
		Definition:  d.definition(),
	}}
	t.info.Permissions.Write = false
	return t
}

func (t *tool) Info() contracts.ToolInfo { return t.info }

// Invoke sends the bare tool name, the way server_mcp_tool::invoke does. The
// out sink stays unused: server_mcp_tool sets support_stream to false.
func (t *tool) Invoke(ctx context.Context, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	return t.e.call(ctx, t.def.name, req.Params)
}
