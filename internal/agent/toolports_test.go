package agent

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/goatherdai/yaah/internal/tools"
)

// The concrete registry must satisfy the engine's port. The assertion lives
// in a test so the production engine package does not import the tool surface
// just to prove the relationship.
var _ ToolRegistry = (*tools.Registry)(nil)

type portFakeTool struct {
	name   string
	desc   string
	schema string
}

func (f portFakeTool) Name() string                                    { return f.name }
func (f portFakeTool) Description() string                             { return f.desc }
func (f portFakeTool) Schema() json.RawMessage                         { return json.RawMessage(f.schema) }
func (f portFakeTool) Execute(context.Context, string) (string, error) { return "", nil }

type portDangerousTool struct{ portFakeTool }

func (portDangerousTool) IsDangerous(string) bool { return true }

type portFakeRegistry struct {
	tools map[string]Tool
	gen   int
}

func (r *portFakeRegistry) Get(name string) Tool { return r.tools[name] }
func (r *portFakeRegistry) List() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (r *portFakeRegistry) Execute(context.Context, string, string) (string, error) { return "", nil }
func (r *portFakeRegistry) Generation() int                                         { return r.gen }

// TestLoopRunsAgainstFakeRegistry is the point of the port: tool-definition
// building and danger classification work with a registry the engine never
// imported, so the loop can be unit-tested without the built-in tool surface.
func TestLoopRunsAgainstFakeRegistry(t *testing.T) {
	reg := &portFakeRegistry{
		tools: map[string]Tool{
			"echo":    portFakeTool{name: "echo", desc: "echo a value", schema: `{"type":"object","properties":{"text":{"type":"string"}}}`},
			"cleanup": portDangerousTool{portFakeTool{name: "cleanup", desc: "remove files", schema: `{"type":"object"}`}},
		},
		gen: 1,
	}
	loop := NewLoop(nil, reg)

	defs := loop.buildToolDefs()
	if len(defs) != 2 {
		t.Fatalf("tool defs = %d, want 2", len(defs))
	}
	if defs[0].Function.Name != "cleanup" || defs[1].Function.Name != "echo" {
		t.Errorf("tool defs not in registry order: %+v", defs)
	}
	var params map[string]any
	if err := json.Unmarshal(defs[1].Function.Parameters, &params); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	if params["additionalProperties"] != false {
		t.Errorf("schema not closed: %s", defs[1].Function.Parameters)
	}
	if !loop.dangerousCall("cleanup", `{}`) {
		t.Error("danger classifier on the fake tool was not consulted")
	}
	if loop.dangerousCall("echo", `{}`) {
		t.Error("a tool without DangerClassifier must not be dangerous")
	}

	loop.toolDefsCache = nil
	reg.gen = 2
	if got := loop.buildToolDefs(); len(got) != 2 {
		t.Errorf("defs after generation bump = %d, want 2", len(got))
	}

	sub := NewSubAgentLoop(nil, reg, "model", "prompt", SubAgentConfig{})
	if sub.Registry != reg {
		t.Error("NewSubAgentLoop did not accept the ToolRegistry port")
	}
}

// TestTypedNilRegistryBehavesAsAbsent pins the regression the port introduced:
// a nil *tools.Registry stored in an interface is non-nil, so plain == nil
// guards would pass and the listing paths would panic. Constructors normalize
// it and the direct-field path checks it with registryNil.
func TestTypedNilRegistryBehavesAsAbsent(t *testing.T) {
	var nilConcrete *tools.Registry
	loop := NewLoop(nil, nilConcrete)
	if !registryNil(loop.Registry) {
		t.Fatal("constructor kept a typed-nil registry as a non-nil port")
	}
	if defs := loop.buildToolDefs(); defs != nil {
		t.Errorf("tool defs with no registry = %v, want nil", defs)
	}
	if got := loop.readOnlyTools(); got != nil {
		t.Errorf("read-only tools with no registry = %v, want nil", got)
	}
	if loop.dangerousCall("echo", `{}`) {
		t.Error("a missing registry must not classify an unknown tool as dangerous")
	}

	sub := NewSubAgentLoop(nil, nilConcrete, "model", "prompt", SubAgentConfig{})
	if !registryNil(sub.Registry) {
		t.Error("NewSubAgentLoop kept a typed-nil registry")
	}

	direct := &Loop{Registry: nilConcrete}
	if defs := direct.buildToolDefs(); defs != nil {
		t.Errorf("direct field: tool defs = %v, want nil", defs)
	}
	if got := direct.readOnlyTools(); got != nil {
		t.Errorf("direct field: read-only tools = %v, want nil", got)
	}
}
