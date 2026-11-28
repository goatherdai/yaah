package yaah

import (
	"testing"

	"github.com/goatherdai/yaah/internal/config"
)

func TestSwitchModel(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.Provider{
		"zai":     {BaseURL: "http://zai.test", APIKey: "k-zai"},
		"zanth":   {BaseURL: "http://zanth.test", APIKey: "k-zanth"},
		"keyless": {},
	}}
	routeProv, _ := makeProvider("zai", cfg.Providers["zai"])
	newSession := func() *agentSession {
		return &agentSession{cfg: cfg, provider: routeProv, providerName: "zai", modelName: "GLM-5"}
	}
	tests := []struct {
		name         string
		arg          string
		wantOK       bool
		wantReply    string
		wantProvider string
		wantModel    string
	}{
		{name: "bare reports the current routing", arg: "", wantOK: true,
			wantReply: "session model: zai/GLM-5 — usage: /model [provider/]model", wantProvider: "zai", wantModel: "GLM-5"},
		{name: "bare model keeps the provider", arg: "GLM-6", wantOK: true,
			wantReply: "session model switched: zai/GLM-6 — effective on the next run", wantProvider: "zai", wantModel: "GLM-6"},
		{name: "provider/model re-points both axes", arg: "zanth/claude-x", wantOK: true,
			wantReply: "session model switched: zanth/claude-x — effective on the next run", wantProvider: "zanth", wantModel: "claude-x"},
		{name: "provider/model trims whitespace", arg: " zanth / claude-x ", wantOK: true,
			wantReply: "session model switched: zanth/claude-x — effective on the next run", wantProvider: "zanth", wantModel: "claude-x"},
		{name: "same routing is a no-op", arg: "zai/GLM-5", wantOK: true,
			wantReply: "session model is already zai/GLM-5", wantProvider: "zai", wantModel: "GLM-5"},
		{name: "unknown provider refused", arg: "nope/mystery", wantOK: false,
			wantReply: `provider "nope" is not in the config — the session stays on zai/GLM-5`, wantProvider: "zai", wantModel: "GLM-5"},
		{name: "unusable provider refused", arg: "keyless/mystery", wantOK: false,
			wantReply: `provider "keyless" is not usable (missing credential or base URL) — the session stays on zai/GLM-5`, wantProvider: "zai", wantModel: "GLM-5"},
		{name: "empty model name refused", arg: "zai/", wantOK: false,
			wantReply: `"zai/" has no model name — usage: /model [provider/]model`, wantProvider: "zai", wantModel: "GLM-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSession()
			got, ok := s.switchModel(tt.arg)
			if ok != tt.wantOK {
				t.Fatalf("switchModel(%q) ok = %v, want %v (reply %q)", tt.arg, ok, tt.wantOK, got)
			}
			if got != tt.wantReply {
				t.Errorf("switchModel(%q) reply = %q, want %q", tt.arg, got, tt.wantReply)
			}
			if provider, model := s.Model(); provider != tt.wantProvider || model != tt.wantModel {
				t.Errorf("switchModel(%q) routing = %q/%q, want %q/%q", tt.arg, provider, model, tt.wantProvider, tt.wantModel)
			}
			if tt.wantOK && tt.arg != "" && !(tt.wantProvider == "zai" && tt.wantModel == "GLM-5") {
				if s.providerName != tt.wantProvider || s.modelName != tt.wantModel {
					t.Errorf("switchModel(%q) session fields = %q/%q, want %q/%q", tt.arg, s.providerName, s.modelName, tt.wantProvider, tt.wantModel)
				}
			}
		})
	}
}

// TestModelCommandNilConfig verifies the bare report works on a
// hand-built session (no cfg), mirroring how the other axis-command
// tests construct sessions.
func TestModelCommandNilConfig(t *testing.T) {
	s := &agentSession{}
	got, ok := s.switchModel("")
	if !ok {
		t.Fatalf("bare switchModel on nil-cfg session refused: %q", got)
	}
	if got != "session model: / — usage: /model [provider/]model" {
		t.Errorf("bare report = %q", got)
	}
	// A switch against a nil cfg must refuse, not panic.
	if _, ok := s.switchModel("zai/GLM-6"); ok {
		t.Error("switchModel with nil cfg must refuse")
	}
}
