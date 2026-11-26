package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
)

// delegationModeFixtures builds role bytes covering the mode vocabulary
// for the delegate-surface filter tests: "primary" (an orchestrator
// persona), "subagent", "all", and a role with no mode key (the
// historical default). The ids are all the same length so the map
// literal needs no gofmt alignment padding.
func delegationModeFixtures() map[string][]byte {
	return map[string][]byte{
		"boss.md": []byte("---\nname: Boss\nmode: primary\ntools: [read]\ndescription: runs the session\n---\nbody"),
		"work.md": []byte("---\nname: Worker\nmode: subagent\ntools: [read]\ndescription: does work\n---\nbody"),
		"flex.md": []byte("---\nname: Flex\nmode: all\ntools: [read]\n---\nbody"),
		"base.md": []byte("---\nname: Legacy\ntools: [read]\n---\nbody"),
	}
}

// schemaRoleEnum extracts the delegate schema's role enum.
func schemaRoleEnum(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var schema struct {
		Properties struct {
			Role struct {
				Enum []string `json:"enum"`
			} `json:"role"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return schema.Properties.Role.Enum
}

// schemaRoleDescription extracts the delegate schema's role parameter
// description (the rendered role list).
func schemaRoleDescription(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var schema struct {
		Properties struct {
			Role struct {
				Description string `json:"description"`
			} `json:"role"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return schema.Properties.Role.Description
}

// TestDelegateSchemaExcludesPrimaryOnlyRoles pins yaah-21uk.4: a role
// with mode "primary" is an orchestrator persona and must vanish from
// the delegate tool's dispatch surface — the schema enum, the rendered
// role descriptions, and execution-time validation. "subagent" (explicit
// or the missing-key default) and "all" stay.
// TestUnconfiguredRoleFilterIsExplicit pins the documented contract: a
// hand-built tool with no Delegatable filter admits every name, while the
// composition root supplies subagent.DelegatableRole to enforce the
// primary-only exclusion in production. This test makes the fail-open
// unwired path a conscious, visible choice rather than an accident.
func TestUnconfiguredRoleFilterIsExplicit(t *testing.T) {
	tool := &DelegateTool{RoleNames: []string{"boss", "work"}}
	if names := tool.roleNames(); !slices.Contains(names, "boss") {
		t.Fatalf("unconfigured filter dropped a role: %v", names)
	}
	tool.Delegatable = func(name string) bool { return name != "boss" }
	if names := tool.roleNames(); slices.Contains(names, "boss") {
		t.Errorf("injected filter did not exclude primary-only role: %v", names)
	}
}

func TestDelegateSchemaExcludesPrimaryOnlyRoles(t *testing.T) {
	reg := subagent.NewRoleRegistry()
	if err := reg.LoadBytes(delegationModeFixtures()); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	subagent.SetDefaultRoleRegistry(reg)
	defer subagent.SetDefaultRoleRegistry(nil)

	names := []string{"boss", "work", "flex", "base"}
	tool := &DelegateTool{
		RoleNames:        names,
		RoleResolver:     func() []string { return names },
		RoleDescriptions: map[string]string{"boss": "runs the session", "work": "does work"},
		Delegatable:      subagent.DelegatableRole,
	}

	t.Run("enum drops primary keeps subagent all and default", func(t *testing.T) {
		enum := schemaRoleEnum(t, tool.Schema())
		if slices.Contains(enum, "boss") {
			t.Errorf("primary-only role leaked into the schema enum: %v", enum)
		}
		for _, want := range []string{"work", "flex", "base"} {
			if !slices.Contains(enum, want) {
				t.Errorf("enum missing delegatable role %q: %v", want, enum)
			}
		}
	})

	t.Run("rendered role descriptions drop primary", func(t *testing.T) {
		desc := schemaRoleDescription(t, tool.Schema())
		if strings.Contains(desc, "boss") {
			t.Errorf("primary-only role leaked into the role description: %q", desc)
		}
		if !strings.Contains(desc, "work: does work") {
			t.Errorf("delegatable role description missing: %q", desc)
		}
	})

	t.Run("execution validation rejects primary", func(t *testing.T) {
		_, err := tool.Execute(context.Background(), `{"description":"d","prompt":"p","role":"boss"}`)
		if err == nil || !IsRoleNotFound(err) {
			t.Fatalf("expected primary-only role to be rejected at dispatch, got %v", err)
		}
	})
}

// TestDelegateSchema_BaseRefInBothBuilders covers the duplicate-builder trap
// (yaah-witj increment B): Schema()'s inline literal serves a tool with no role
// names and BuildDelegateSchemaFiltered serves a configured one, and the two
// property bodies were byte-for-byte duplicates. A parameter added to only one
// is an environment-dependent silent divergence, so both are asserted here.
func TestDelegateSchema_BaseRefInBothBuilders(t *testing.T) {
	type prop struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	read := func(t *testing.T, raw json.RawMessage) prop {
		t.Helper()
		var schema struct {
			Properties map[string]prop `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("unmarshal schema: %v", err)
		}
		p, ok := schema.Properties["base_ref"]
		if !ok {
			t.Fatalf("schema has no base_ref property: %v", schema.Properties)
		}
		return p
	}

	t.Run("inline builder", func(t *testing.T) {
		p := read(t, (&DelegateTool{}).Schema())
		if p.Type != "string" {
			t.Fatalf("base_ref = %+v, want a string property", p)
		}
		if p.Description != baseRefParamDescription {
			t.Fatalf("description = %q, want the shared const", p.Description)
		}
	})
	t.Run("filtered builder", func(t *testing.T) {
		p := read(t, BuildDelegateSchemaFiltered([]string{"developer"}, nil, nil))
		if p.Type != "string" {
			t.Fatalf("base_ref = %+v, want a string property", p)
		}
		if p.Description != baseRefParamDescription {
			t.Fatalf("description = %q, want the shared const (the two builders must not drift)", p.Description)
		}
	})
}
