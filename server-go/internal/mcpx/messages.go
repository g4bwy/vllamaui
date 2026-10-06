package mcpx

// Every string an operator or the caller can see is here, so a grep for a
// message finds the code that prints it. The C++ texts come from
// tools/server/server-mcp.cpp and server-tools.cpp.
const (
	// Prefixes of the fatal Load errors. A caller must stop when it gets one.
	prefixOpenConfig  = "failed to open MCP config file: "
	prefixParseConfig = "failed to parse MCP config JSON: "
	// errDuplicateServer guards a hand built Config: Load never returns one.
	errDuplicateServer = "MCP config: duplicate server name '%s'"
)

// Log messages.
const (
	msgNoServers     = "MCP config: no servers found in JSON"
	msgDuplicate     = "MCP config: duplicate server name '%s', skipping"
	msgNoCommand     = "MCP server '%s' has no command, skipping"
	msgWarmup        = "MCP warmup: '%s' discovered %d tools"
	msgRefreshed     = "MCP '%s' rediscovered %d tools after a list change"
	msgWarmupSpawn   = "MCP warmup: failed to spawn '%s': %s"
	msgNotAlive      = "MCP '%s' is no longer alive: %s"
	msgListFailed    = "MCP '%s': tools/list failed: %s"
	msgFailedStart   = "MCP '%s': failed to start: %s"
	msgCollision     = "MCP tool %q from server %q collides with an existing tool, skipping"
	msgAdded         = "Added %d MCP tools"
	msgDroppedParts  = "MCP tool %q discarded %d non-text content part(s)"
	msgDroppedSchema = "MCP tool %q ignored structuredContent"
)

// Result.Error texts, as llama-server reports them on POST /tools.
const (
	textUnavailable = "MCP server unavailable: "
	textClosed      = "transport closed"
	textCancelled   = "cancelled"
	textTimedOut    = "request timed out"
	textUnknown     = "unknown error"
	textInvalid     = "invalid response from MCP server"
	textToolFailed  = "MCP tool returned an error"
)
