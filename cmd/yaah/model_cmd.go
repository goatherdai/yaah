package yaah

import (
	"fmt"
	"strings"
)

// modelStatusLine renders /model with no argument: the session's current
// routing.
func (s *agentSession) modelStatusLine() string {
	return fmt.Sprintf("session model: %s/%s", s.ProviderName(), s.ModelName())
}

// switchModel handles REPL /model (parity with the TUI's :model picker and
// ACP session/set_config_option): an empty argument reports the current
// routing; otherwise the session switches. "provider/model" re-points both
// axes, a bare model name keeps the current provider. A provider the config
// does not define, or cannot construct, is refused — SetModel's
// refuse-and-warn guard keeps the session on its current routing and the
// reply reports that instead of confirming a switch that did not happen.
// Like every other axis switch, the new routing is effective at the NEXT
// run; an in-flight turn keeps the model it started with.
func (s *agentSession) switchModel(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return s.modelStatusLine() + " — usage: /model [provider/]model", true
	}
	provider, model := s.ProviderName(), arg
	if name, rest, ok := strings.Cut(arg, "/"); ok {
		provider, model = strings.TrimSpace(name), strings.TrimSpace(rest)
	}
	if model == "" {
		return fmt.Sprintf("%q has no model name — usage: /model [provider/]model", arg), false
	}
	if provider == s.ProviderName() && model == s.ModelName() {
		return fmt.Sprintf("session model is already %s/%s", provider, model), true
	}
	defined := false
	if s.cfg != nil {
		_, defined = s.cfg.Providers[provider]
	}
	if !defined {
		return fmt.Sprintf("provider %q is not in the config — the session stays on %s/%s", provider, s.ProviderName(), s.ModelName()), false
	}
	if !s.SetModel(provider, model) {
		return fmt.Sprintf("provider %q is not usable (missing credential or base URL) — the session stays on %s/%s", provider, s.ProviderName(), s.ModelName()), false
	}
	return fmt.Sprintf("session model switched: %s/%s — effective on the next run", provider, model), true
}

// modelCommand handles REPL /model: no argument reports the current
// provider/model; a routing validates and SWITCHES this session. Refusals
// and confirmations both go through the same status sink the other axis
// commands use.
func (s *agentSession) modelCommand(arg string) {
	msg := s.personaMsg()
	reply, ok := s.switchModel(arg)
	if !ok {
		msg("model: " + reply)
		return
	}
	msg(reply)
}
