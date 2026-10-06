package toolsapi

import (
	"llama-webui/server/internal/contracts"
)

// Merge folds several registries into one. The first one that has a name wins,
// which is how built-in tools keep their names against an MCP tool that would
// otherwise collide with them. llama-server does the same in server_tools::
// setup, where a colliding MCP tool is skipped with a warning.
//
// The result is a snapshot: it does not follow a registry that gains or loses
// tools afterwards.
func Merge(regs ...contracts.Registry) contracts.Registry {
	m := &merged{byName: map[string]contracts.Tool{}, owner: map[string]contracts.Registry{}}
	for _, reg := range regs {
		if reg == nil {
			continue
		}
		for _, info := range reg.List() {
			if _, taken := m.byName[info.Tool]; taken {
				continue
			}
			tool, ok := reg.Get(info.Tool)
			if !ok {
				continue
			}
			m.byName[info.Tool] = tool
			m.owner[info.Tool] = reg
			m.order = append(m.order, info.Tool)
		}
	}
	return m
}

// merged is the registry Merge builds.
type merged struct {
	order  []string
	byName map[string]contracts.Tool
	owner  map[string]contracts.Registry
}

var (
	_ contracts.Registry = (*merged)(nil)
	_ streamer           = (*merged)(nil)
)

// List keeps the order of the registries it was built from: the tools of the
// first one, then the ones the first did not already claim.
func (m *merged) List() []contracts.ToolInfo {
	out := make([]contracts.ToolInfo, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.byName[name].Info())
	}
	return out
}

func (m *merged) Get(name string) (contracts.Tool, bool) {
	t, ok := m.byName[name]
	return t, ok
}

// SupportsStream asks the registry that owns the tool, so wrapping a registry
// that declares its capability does not lose it.
func (m *merged) SupportsStream(name string) bool {
	if s, ok := m.owner[name].(streamer); ok {
		return s.SupportsStream(name)
	}
	return name == execShellCommand
}
