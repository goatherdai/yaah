package command

import "strings"

// Completion is one typeahead suggestion for the composer.
type Completion struct {
	Label  string // command name, without the leading ':'
	Desc   string // one-line description shown beside the name
	Insert string // text to place in the composer, e.g. ":steer "
}

// Complete returns typeahead suggestions for text typed in the composer.
//
// Suggestions are offered only while a command *name* is being typed: text that
// does not start with ':' — or that already contains a space, meaning an
// argument is in progress — yields none.
func Complete(text string, entries []Entry) []Completion {
	body, ok := strings.CutPrefix(text, ":")
	if !ok || strings.ContainsAny(body, " \t") {
		return nil
	}
	prefix := strings.ToLower(body)
	var out, exact []Completion
	for _, e := range entries {
		if e.Label == "" || !strings.HasPrefix(strings.ToLower(e.Label), prefix) {
			continue
		}
		insert := ":" + e.Label
		if RequiresArg(e.Cmd) || e.Cmd == CmdModel {
			// Leave the cursor where the argument starts.
			insert += " "
		}
		c := Completion{Label: e.Label, Desc: e.Desc, Insert: insert}
		// An exact name match outranks longer commands that merely share
		// the prefix: ":mode" must select "mode", not "model", even
		// though the palette lists "model" first. This is the standard
		// typeahead rule — finishing a name should not shadow it.
		if strings.EqualFold(e.Label, body) {
			exact = append(exact, c)
		} else {
			out = append(out, c)
		}
	}
	return append(exact, out...)
}
