package toolsapi

import (
	"llama-webui/server/internal/contracts"
)

// Merge folds several registries into one. The first one that has a name wins,
// which is how built-in tools keep their names against an MCP tool that would
// otherwise collide with them. llama-server does the same in server_tools::
// setup, where a colliding MCP tool is skipped with a warning.
//
// The result is live: it keeps the registries and asks them on every call, so a
// registry that gains or loses tools afterwards (an MCP refresh, a chat tool
// registered at first use) shows up without building the merge again.
func Merge(regs ...contracts.Registry) contracts.Registry {
	m := &merged{}
	for _, reg := range regs {
		if reg != nil {
			m.registries = append(m.registries, reg)
		}
	}
	return m
}

// merged is the registry Merge builds. It holds only the ordered registries, so
// nothing about their contents is frozen at startup.
type merged struct {
	registries []contracts.Registry // fixed by Merge, read-only after that
}

var (
	_ contracts.Registry = (*merged)(nil)
	_ streamer           = (*merged)(nil)
)

// List walks the registries in order: the tools of the first one, then the ones
// the first did not already claim.
func (m *merged) List() []contracts.ToolInfo {
	snap := m.scan()
	out := make([]contracts.ToolInfo, 0, len(snap.names))
	for _, name := range snap.names {
		out = append(out, snap.tools[name].Info())
	}
	return out
}

func (m *merged) Get(name string) (contracts.Tool, bool) {
	tool, ok := m.scan().tools[name]
	return tool, ok
}

// SupportsStream asks the registry that owns the tool, so wrapping a registry
// that declares its capability does not lose it.
func (m *merged) SupportsStream(name string) bool {
	snap := m.scan()
	owner, ok := snap.owners[name]
	if !ok {
		owner = nil
	}
	if s, ok := owner.(streamer); ok {
		return s.SupportsStream(name)
	}
	return name == execShellCommand
}

// snapshot is one first-wins pass over the registries.
type snapshot struct {
	names  []string                      // the order the tools were claimed in
	tools  map[string]contracts.Tool     // name to tool
	owners map[string]contracts.Registry // name to the registry that won it
}

// scan takes each name from the first registry that both lists it and answers
// for it. A name a registry lists but cannot return is skipped, so a later
// registry can still offer it.
func (m *merged) scan() *snapshot {
	snap := &snapshot{
		tools:  make(map[string]contracts.Tool),
		owners: make(map[string]contracts.Registry),
	}
	for _, reg := range m.registries {
		for _, info := range reg.List() {
			if _, taken := snap.tools[info.Tool]; taken {
				continue
			}
			tool, ok := reg.Get(info.Tool)
			if !ok {
				continue
			}
			snap.tools[info.Tool] = tool
			snap.owners[info.Tool] = reg
			snap.names = append(snap.names, info.Tool)
		}
	}
	return snap
}
