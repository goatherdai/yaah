package acpclient

import "testing"

func TestRegistryResolveAndNames(t *testing.T) {
	r := NewRegistry(map[string]Manifest{
		"vibe": {Name: "vibe", Command: "vibe-acp"},
		"zed":  {Name: "zed", Command: "some-agent"},
	})
	if r.Resolve("vibe") == nil {
		t.Fatal("vibe should resolve")
	}
	if r.Resolve("missing") != nil {
		t.Fatal("unknown agent should resolve to nil")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "vibe" || names[1] != "zed" {
		t.Fatalf("names = %v", names)
	}
}

func TestNilRegistry(t *testing.T) {
	var r *Registry
	if r.Resolve("vibe") != nil || r.Names() != nil {
		t.Fatal("nil registry should be inert")
	}
}
