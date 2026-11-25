package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// The dangerous-command gate shared by the shell-running tools: `bash`, and
// `background_process` start/restart. Both execute an arbitrary shell line, and
// both refuse the most blatant destructive commands even under `approval:
// allow`. This is a best-effort guard, NOT a security boundary: model-generated
// shell trivially evades a pattern list (whitespace, variables, indirection,
// decoders). Real protection is the approval gate (config.Approval).
//
// The guard used to be a plain substring scan over the whole line (isDangerous),
// and a substring scan cannot tell a verb from its data. That refused read-only
// work for the vocabulary inside a quoted argument — `grep -rn 'halt'` and
// `git commit -m "... halt ..."` — which pushed callers into writing the text to
// a file and passing it by reference (bead yaah-0lx8). So the gate now
// classifies the line first: it splits it into the segments of a pipeline or
// chain, takes each segment's executing verb, and asks whether that invocation
// only reads. A read-only invocation's arguments are data and are not scanned;
// every other segment is scanned exactly as before.
//
// The classifier is deliberately one-sided. It exempts only an invocation it
// positively recognizes as read-only, and anything it cannot account for —
// command substitution, process substitution, backticks, an unterminated quote,
// a leading `(`, a verb it does not know — falls back to scanning the whole
// line, which is precisely the previous behaviour. A classification failure can
// therefore only keep a denial, never create an escape from one.
//
// Three rules keep the exemption honest, each added after review found an escape
// through it (yaah-0lx8 review round 1). An exemption covers ONE segment, so a
// segment whose verb is an interpreter or a wrapper withdraws every other
// segment's exemption and the raw whole line is scanned instead: `echo 'rm -rf
// /' | sh` runs the echo's argument as a command. An interpreter fed by a pipe
// with no script of its own (`cat notes.txt | bash`) is refused outright,
// because its input IS the program and nothing in the line can show otherwise.
// A read-only verb counts as read-only only while its own flags keep it so —
// sort's --compress-program and -o, tree's -o, git's -c and --output, sed's
// write and execute commands — so an exemption is narrowed in the classifier
// rather than trusted from the verb's name. And a device path is refused
// wherever a scanned segment names one (`sort -o /dev/sda`, `sed 'w /dev/sda'`
// write a disk with no shell redirect for checkRedirects to see), which is also
// the rule that covers a line the tokenizer cannot classify at all.
//
// Known residual (deliberately out of scope here): a here-document body is not
// modelled, so prose inside `cat > f <<'EOF' ... EOF` is still scanned as a
// command line. Supporting it means proving the consuming verb is inert before
// skipping the body (`sh <<EOF` must stay scanned), which is more parser than
// this gate needs; it is status quo, not a regression.

// dangerousPatterns is a coarse deny-list of obviously destructive shell
// patterns. Patterns are matched case-insensitively as substrings, so every
// entry must be written in lower case (a mixed-case entry can never fire —
// see TestDangerousPatternsAreLowercase).
var dangerousPatterns = []string{
	"rm -rf /", "rm -rf ~", "rm -rf .",
	"shutdown", "reboot", "halt",
	"mkfs", "mkswap",
	"dd if=", ":(){ :|:& };:",
	"chmod 777 /", "chmod -r 777 /", "chown -r",
	"remove-item -recurse -force c:\\",
	"format-volume", "stop-computer", "restart-computer",
	"clear-disk", "initialize-disk",
}

// scanDenyPatterns returns the first deny-list pattern present in text.
func scanDenyPatterns(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, pattern := range dangerousPatterns {
		if strings.Contains(lower, pattern) {
			return pattern, true
		}
	}
	return "", false
}

// isDangerous reports whether cmd matches a deny-list pattern anywhere in the
// line. It is the blunt scan: the fail-closed fallback for a shell line the verb
// classifier cannot account for, and the only scan the PowerShell tool uses (a
// PowerShell line is not POSIX shell, so tokenizing it would be guessing).
func isDangerous(cmd string) bool {
	_, bad := scanDenyPatterns(cmd)
	return bad
}

// shellDenial explains a refusal: which pattern or structural rule fired, on
// which verb, in which segment. Naming them is the difference between a refusal
// the caller can act on and a wall.
type shellDenial struct {
	Pattern string // deny-list pattern that matched, when one did
	Rule    string // structural rule that fired, when one did
	Verb    string // executing verb of the offending segment
	Segment string // offending segment, clipped for display
}

// String renders the denial for an error message.
func (d shellDenial) String() string {
	var b strings.Builder
	if d.Pattern != "" {
		fmt.Fprintf(&b, "pattern %q", d.Pattern)
	}
	if d.Rule != "" {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Rule)
	}
	if b.Len() == 0 {
		b.WriteString("deny-list match")
	}
	if d.Verb != "" {
		fmt.Fprintf(&b, " on verb %q", d.Verb)
	}
	if d.Segment != "" {
		fmt.Fprintf(&b, " in segment %q", d.Segment)
	}
	return b.String()
}

// clipSegment bounds a segment for display in a refusal.
func clipSegment(seg string) string {
	return truncateCmd(strings.TrimSpace(seg), 80)
}

// denyShellCommand is the gate: it reports whether the shell line must be
// refused, and why. Both shell-running tools go through it.
func denyShellCommand(cmd string) (shellDenial, bool) {
	segs, ok := splitShellSegments(cmd)
	if !ok {
		// Unclassifiable line: fail closed with the old substring scan, plus the
		// device rule, which grouping would otherwise hide — `{ echo hi >
		// /dev/sda; }` tokenizes as nothing at all.
		if pattern, bad := scanDenyPatterns(cmd); bad {
			return shellDenial{Pattern: pattern}, true
		}
		if device, bad := unsafeDeviceInLine(cmd); bad {
			return shellDenial{Rule: fmt.Sprintf("device path %q", device)}, true
		}
		return shellDenial{}, false
	}
	if denial, bad := checkCodeVerbs(cmd, segs); bad {
		return denial, true
	}
	for _, seg := range segs {
		if denial, bad := classifySegment(seg); bad {
			return denial, true
		}
	}
	return shellDenial{}, false
}

// shTok is one token of a shell command line: a word, or an operator.
type shTok struct {
	text string // word text, quoting removed and escapes resolved ("" for an operator)
	raw  string // the source slice, exactly as written
	off  int    // byte offset of raw within the command line
	op   string // operator, when this token is one (">", "&&", "|", ...)
}

// isWord reports whether the token is a word rather than an operator.
func (t shTok) isWord() bool { return t.op == "" }

// shellSegment is one command of a pipeline or chain: the tokens between
// separators, plus the segment's own source text.
type shellSegment struct {
	toks  []shTok // the segment's tokens, separators excluded
	raw   string  // the segment's source text, exactly as written
	start int     // byte offset of raw within the command line
}

// isSegmentSeparator reports whether an operator starts a new command.
func isSegmentSeparator(op string) bool {
	switch op {
	case ";", "\n", ";;", "&&", "||", "|", "|&", "&":
		return true
	}
	return false
}

// splitShellSegments splits a shell line into its pipeline/chain segments. It
// reports ok=false when the line cannot be tokenized with confidence, which
// makes the caller fall back to the blunt scan.
func splitShellSegments(cmd string) ([]shellSegment, bool) {
	toks, ok := tokenizeShell(cmd)
	if !ok {
		return nil, false
	}
	var (
		segs []shellSegment
		cur  []shTok
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		first, last := cur[0], cur[len(cur)-1]
		end := last.off + len(last.raw)
		segs = append(segs, shellSegment{toks: cur, raw: cmd[first.off:end], start: first.off})
		cur = nil
	}
	for _, tok := range toks {
		if !tok.isWord() && isSegmentSeparator(tok.op) {
			flush()
			continue
		}
		cur = append(cur, tok)
	}
	flush()
	return segs, true
}

// tokenizeShell splits a shell line into words and operators. It is deliberately
// small — single quotes, double quotes, backslash escapes, comments and the
// operators that separate and redirect commands — and it reports ok=false for
// anything whose meaning depends on expansion it does not model, so a guess can
// never become an exemption.
func tokenizeShell(cmd string) ([]shTok, bool) {
	var toks []shTok
	i, n := 0, len(cmd)
	for i < n {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case c == '\n':
			toks = append(toks, shTok{op: "\n", raw: "\n", off: i})
			i++
			continue
		case c == '#':
			// A comment runs to the end of the line and runs nothing. POSIX
			// only treats '#' as a comment where a word could start, which is
			// exactly here; anywhere else it is word content (below).
			for i < n && cmd[i] != '\n' {
				i++
			}
			continue
		}
		if op, size := shellOperator(cmd[i:]); op != "" {
			toks = append(toks, shTok{op: op, raw: cmd[i : i+size], off: i})
			i += size
			continue
		}
		word, size, ok := shellWord(cmd[i:])
		if !ok {
			return nil, false
		}
		toks = append(toks, shTok{text: word, raw: cmd[i : i+size], off: i})
		i += size
	}
	return toks, true
}

// shellOperators lists the operators the tokenizer recognizes. Order matters:
// longer operators are matched first so "&&" wins over "&", ">>" over ">".
var shellOperators = []string{
	"&&", "||", ";;", ">>", ">|", "&>", "<&", ">&", "|&", "<<",
	";", "|", "&", "<", ">", "\n",
}

// shellOperator returns the operator at the head of s and its byte length.
func shellOperator(s string) (string, int) {
	for _, op := range shellOperators {
		if strings.HasPrefix(s, op) {
			return op, len(op)
		}
	}
	return "", 0
}

// shellWord scans one shell word at the head of s, returning its text (quotes
// removed, escapes resolved) and the bytes consumed. It reports ok=false for a
// construct that can run code — $(...), ${...}, backticks, unquoted grouping
// parens or braces — and for an unterminated quote.
func shellWord(s string) (string, int, bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			break
		}
		if op, _ := shellOperator(s[i:]); op != "" {
			break
		}
		switch c {
		case '(', ')', '{', '}', '`':
			return "", 0, false
		case '\'':
			// Single quotes are literal: nothing inside expands, so arbitrary
			// vocabulary here is genuinely inert data.
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", 0, false
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case '"':
			j := i + 1
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					j++
					closed = true
					break
				}
				if runsCode(s[j:]) {
					return "", 0, false
				}
				b.WriteByte(s[j])
				j++
			}
			if !closed {
				return "", 0, false
			}
			i = j
		case '\\':
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		default:
			if runsCode(s[i:]) {
				return "", 0, false
			}
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, true
}

// runsCode reports whether s starts with a construct that can run a command:
// $(...), ${...} or a backtick.
func runsCode(s string) bool {
	if s == "" {
		return false
	}
	if len(s) >= 2 && s[0] == '$' && (s[1] == '(' || s[1] == '{') {
		return true
	}
	return s[0] == '`'
}

// segmentVerb returns the index in toks of the segment's executing verb, which
// is its first word that is not a leading environment assignment
// (`FOO=bar grep ...`). -1 means the segment runs no command of its own.
func segmentVerb(toks []shTok) int {
	for i, tok := range toks {
		if !tok.isWord() {
			continue
		}
		if isEnvAssignment(tok.text) {
			continue
		}
		return i
	}
	return -1
}

// isEnvAssignment reports whether a word is a NAME=value environment prefix.
func isEnvAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := word[i]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// wordTexts returns the text of the word tokens in toks.
func wordTexts(toks []shTok) []string {
	words := make([]string, 0, len(toks))
	for _, tok := range toks {
		if tok.isWord() {
			words = append(words, tok.text)
		}
	}
	return words
}

// codeVerbs are the verbs that run something they are handed rather than
// treating it as data: shells and language interpreters, whose argument or
// standard input is a program, and wrappers, whose arguments are another
// command. A line containing one cannot be cleared segment by segment, because
// the data fed to it is exactly where the code hides.
var codeVerbs = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"awk": true, "gawk": true,
	"xargs": true, "env": true, "eval": true, "exec": true, "source": true, ".": true,
}

// stdinCodeVerbs are the code verbs that read their program from standard input
// when they are given no program of their own.
var stdinCodeVerbs = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"awk": true, "gawk": true,
}

// checkCodeVerbs refuses a line that hands code to an interpreter or wrapper. A
// code verb withdraws the data exemption from every other segment — `echo 'rm
// -rf /' | sh` is a command line whatever the echo's argument looks like — so
// the RAW line is scanned, and an interpreter fed by a pipe with no program of
// its own (`cat notes.txt | bash`) is refused outright, because its input is the
// program.
func checkCodeVerbs(cmd string, segs []shellSegment) (shellDenial, bool) {
	for i, seg := range segs {
		verbIdx := segmentVerb(seg.toks)
		if verbIdx < 0 {
			continue
		}
		verb := seg.toks[verbIdx].text
		if !codeVerbs[verb] {
			continue
		}
		if pattern, bad := scanDenyPatterns(cmd); bad {
			return shellDenial{Pattern: pattern, Verb: verb, Segment: clipSegment(seg.raw)}, true
		}
		if i > 0 && feedsInterpreter(verb, wordTexts(seg.toks[verbIdx+1:])) && pipedInto(cmd, segs, i) {
			return shellDenial{
				Rule:    fmt.Sprintf("data piped into interpreter %q", verb),
				Verb:    verb,
				Segment: clipSegment(seg.raw),
			}, true
		}
	}
	return shellDenial{}, false
}

// feedsInterpreter reports whether the invocation reads its program from
// standard input: an interpreter given no program of its own, or a wrapper whose
// last word is such an interpreter (`env bash`, `xargs perl`).
func feedsInterpreter(verb string, args []string) bool {
	if stdinCodeVerbs[verb] {
		for _, arg := range args {
			if isEnvAssignment(arg) {
				continue
			}
			if !strings.HasPrefix(arg, "-") {
				return false // a positional argument supplies the program
			}
			for _, flag := range []string{"-c", "-e", "--eval", "-m", "--module"} {
				if strings.HasPrefix(arg, flag) {
					return false // the flag supplies the program
				}
			}
		}
		return true
	}
	// A wrapper hands its arguments to another command; the last word that is
	// not a flag or an assignment is the command it will run.
	for i := len(args) - 1; i >= 0; i-- {
		arg := args[i]
		if strings.HasPrefix(arg, "-") || isEnvAssignment(arg) {
			continue
		}
		return stdinCodeVerbs[arg]
	}
	return false
}

// pipedInto reports whether the segment at index i is fed by a pipe rather than
// started by a chain operator.
func pipedInto(cmd string, segs []shellSegment, i int) bool {
	if i <= 0 || i >= len(segs) {
		return false
	}
	prev, cur := segs[i-1], segs[i]
	end := prev.start + len(prev.raw)
	if end < 0 || end > len(cmd) || cur.start > len(cmd) || end > cur.start {
		return false
	}
	return strings.Contains(cmd[end:cur.start], "|")
}

// classifySegment refuses one segment or clears it.
func classifySegment(seg shellSegment) (shellDenial, bool) {
	// Redirect targets are checked for every segment, read-only ones included:
	// `grep x f > /dev/sda` reads nothing dangerous but destroys a disk.
	if denial, bad := checkRedirects(seg); bad {
		return denial, true
	}
	verbIdx := segmentVerb(seg.toks)
	if verbIdx < 0 {
		// Only assignments and redirects: no program runs.
		return shellDenial{}, false
	}
	verb := seg.toks[verbIdx].text
	args := wordTexts(seg.toks[verbIdx+1:])
	if classify, known := readOnlyInvocations[verb]; known && classify(args) {
		// A read-only invocation: its arguments are patterns, paths and text,
		// never commands, so the deny-list does not apply to them.
		return shellDenial{}, false
	}
	// Message values are prose, so they are blanked before anything scans the
	// segment: a pattern or a device path quoted in a commit message is neither
	// a command nor a write.
	scanned := redactMessageArgs(seg, verb, seg.toks[verbIdx+1:])
	if pattern, bad := scanDenyPatterns(scanned); bad {
		return shellDenial{Pattern: pattern, Verb: verb, Segment: clipSegment(seg.raw)}, true
	}
	// A device path is a write whichever argument carries it: `sort -o
	// /dev/sda` and `sed 'w /dev/sda'` reach the disk without a redirect for
	// checkRedirects to see.
	if device, bad := unsafeDeviceInLine(scanned); bad {
		return shellDenial{
			Rule:    fmt.Sprintf("device path %q", device),
			Verb:    verb,
			Segment: clipSegment(seg.raw),
		}, true
	}
	if denial, bad := checkDDTargets(verb, args, seg); bad {
		return denial, true
	}
	if ref, bad := forcePushToProtectedRef(verb, args); bad {
		return shellDenial{
			Rule:    fmt.Sprintf("force push to protected ref %q", ref),
			Verb:    verb,
			Segment: clipSegment(seg.raw),
		}, true
	}
	return shellDenial{}, false
}

// safeDeviceSinks are the /dev paths a redirect may write to: they sink or
// discard output instead of holding state.
var safeDeviceSinks = []string{
	"/dev/null", "/dev/zero", "/dev/stdout", "/dev/stderr", "/dev/tty",
	"/dev/full", "/dev/random", "/dev/urandom", "/dev/fd/", "/dev/pts/",
	"/dev/shm/",
}

// isWriteRedirect reports whether an operator opens a file for writing.
func isWriteRedirect(op string) bool {
	switch op {
	case ">", ">>", ">|", "&>", "&>>", ">&":
		return true
	}
	return false
}

// isDigits reports whether s is a bare file-descriptor number (the target of a
// `2>&1` style dup, which writes no file).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// unsafeDevicePath reports whether p names a device node that must not be
// written to. Writing to a device does not fail the way writing to a file does:
// it destroys whatever holds the device.
func unsafeDevicePath(p string) bool {
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	for _, sink := range safeDeviceSinks {
		if p == sink || strings.HasPrefix(p, sink) {
			return false
		}
	}
	return true
}

// devicePathRE matches a candidate device path anywhere in a shell line: the
// token starts at /dev/ and runs to a shell, flag or path delimiter.
var devicePathRE = regexp.MustCompile(`/dev/[^\s;&|<>"'=:,()]+`)

// unsafeDeviceInLine returns the first device node named anywhere in text that
// must not be written to. It is the whole-text form of checkRedirects: it does
// not care whether a shell operator marks the write, because a read-only verb's
// own output flag (`sort -o /dev/sda`) and a sed write command (`sed 'w
// /dev/sda'`) reach the same disk, and a line the tokenizer refuses outright is
// still checked with it.
func unsafeDeviceInLine(text string) (string, bool) {
	for _, p := range devicePathRE.FindAllString(text, -1) {
		if unsafeDevicePath(p) {
			return p, true
		}
	}
	return "", false
}

// checkRedirects refuses a redirect into a device node: `> /dev/sda` truncates a
// disk, and no read-only verb in front of it makes that safe.
func checkRedirects(seg shellSegment) (shellDenial, bool) {
	for i, tok := range seg.toks {
		if !isWriteRedirect(tok.op) {
			continue
		}
		if i+1 >= len(seg.toks) {
			continue
		}
		target := seg.toks[i+1]
		if !target.isWord() {
			continue
		}
		if tok.op == ">&" && isDigits(target.text) {
			continue // file-descriptor dup: `2>&1`
		}
		if !unsafeDevicePath(target.text) {
			continue
		}
		return shellDenial{
			Rule:    fmt.Sprintf("redirect to device %q", target.text),
			Segment: clipSegment(seg.raw),
		}, true
	}
	return shellDenial{}, false
}

// checkDDTargets refuses `dd of=/dev/sdX`: dd's own output argument is a write
// target that no shell operator marks as one.
func checkDDTargets(verb string, args []string, seg shellSegment) (shellDenial, bool) {
	if verb != "dd" {
		return shellDenial{}, false
	}
	for _, arg := range args {
		target, ok := strings.CutPrefix(arg, "of=")
		if !ok || !unsafeDevicePath(target) {
			continue
		}
		return shellDenial{
			Rule:    fmt.Sprintf("dd writes to device %q", target),
			Verb:    verb,
			Segment: clipSegment(seg.raw),
		}, true
	}
	return shellDenial{}, false
}

// messageArgs maps a verb to the flags whose VALUE is prose the caller wrote — a
// commit message, a pull-request body, an issue description. That value is
// committed, posted or stored, never executed, so it is data and is not
// pattern-scanned. Everything else in the invocation still is.
var messageArgs = map[string][]string{
	"git": {"-m", "--message", "-F", "--file"},
	"gh":  {"--body", "-b", "--title", "-t", "--body-file"},
	"bd":  {"-d", "--description", "--notes", "--design", "--acceptance", "--reason"},
}

// redactMessageArgs returns the segment's source text with message-argument
// values blanked out, so a pattern inside prose cannot be mistaken for a
// command.
func redactMessageArgs(seg shellSegment, verb string, args []shTok) string {
	flags := messageArgs[verb]
	if len(flags) == 0 {
		return seg.raw
	}
	redacted := []byte(seg.raw)
	blank := func(tok shTok) {
		lo := tok.off - seg.start
		hi := lo + len(tok.raw)
		if lo < 0 || hi > len(redacted) {
			return
		}
		for i := lo; i < hi; i++ {
			redacted[i] = ' '
		}
	}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !tok.isWord() {
			continue
		}
		for _, flag := range flags {
			switch {
			case tok.text == flag:
				// Separate value: `--message "..."`
				if i+1 < len(args) && args[i+1].isWord() {
					blank(args[i+1])
					i++
				}
			case strings.HasPrefix(tok.text, flag+"="):
				// Attached form: `--message="..."`
				blank(tok)
			case !strings.HasPrefix(flag, "--") && strings.HasPrefix(tok.text, flag) && len(tok.text) > len(flag):
				// Attached form: `-m"..."`, `-mseparate-word`. The flag itself
				// carries no pattern, so the whole token is blanked.
				blank(tok)
			case isShortFlagCluster(tok.text, flag):
				// Cluster form with the value in the next argument: `-am "..."`,
				// `-anm "..."`. Without this the message is scanned and its prose
				// mistaken for a command.
				if i+1 < len(args) && args[i+1].isWord() {
					blank(args[i+1])
					i++
				}
			}
		}
	}
	return string(redacted)
}

// isShortFlagCluster reports whether word is a single-dash cluster of letters
// whose last letter is a value-taking short flag (`-am`, `-anm` for `-m`), the
// form in which the flag's value arrives as the next argument.
func isShortFlagCluster(word, flag string) bool {
	if len(flag) != 2 || flag[0] != '-' || flag[1] == '-' {
		return false
	}
	if len(word) < 3 || word[0] != '-' || word[1] == '-' {
		return false
	}
	if word[len(word)-1] != flag[1] {
		return false
	}
	for i := 1; i < len(word); i++ {
		c := word[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// protectedPushRefs are the refs a force push may never target: rewriting the
// mainline is not a mistake that can be walked back.
var protectedPushRefs = map[string]bool{
	"main": true, "master": true,
	"refs/heads/main": true, "refs/heads/master": true,
}

// forcePushToProtectedRef reports the protected ref a force push targets, if
// any. The deny-list never had this rule; the bead names a force push to the
// mainline as a hard refusal, so it is enforced structurally (verb plus the
// role of its arguments) instead of as a substring.
func forcePushToProtectedRef(verb string, args []string) (string, bool) {
	if verb != "git" {
		return "", false
	}
	sub, rest, ok := gitSubcommand(args)
	if !ok || sub != "push" {
		return "", false
	}
	forced := false
	for _, arg := range rest {
		switch arg {
		case "-f", "--force", "--force-with-lease", "--force-if-includes":
			forced = true
		case "--mirror":
			// --mirror rewrites every ref on the remote, mainline included, so
			// it cannot be narrowed to a safe target.
			return "every ref (--mirror)", true
		}
		if strings.HasPrefix(arg, "--force-with-lease=") {
			forced = true
		}
	}
	if !forced {
		return "", false
	}
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		ref := arg
		if idx := strings.LastIndexByte(ref, ':'); idx >= 0 {
			ref = ref[idx+1:]
		}
		ref = strings.TrimPrefix(ref, "+")
		if protectedPushRefs[ref] {
			return ref, true
		}
	}
	return "", false
}

// gitGlobalValueFlags are the git options that consume the next argument, so the
// subcommand scan skips over their value instead of reading it as the
// subcommand.
var gitGlobalValueFlags = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true,
}

// gitSubcommand returns a git invocation's subcommand and the arguments after
// it.
func gitSubcommand(args []string) (string, []string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if gitGlobalValueFlags[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg, args[i+1:], true
	}
	return "", nil, false
}

// gitBranchListFlags are the branch options that mutate rather than list.
var gitBranchListFlags = map[string]bool{
	"-d": true, "-D": true, "--delete": true, "-m": true, "-M": true, "--move": true,
	"-c": true, "-C": true, "--copy": true, "-f": true, "--force": true,
	"-u": true, "--set-upstream-to": true, "--unset-upstream": true,
	"--edit-description": true, "-t": true, "--track": true, "--no-track": true,
}

// gitTagListFlags are the tag options that create, sign or delete a tag.
var gitTagListFlags = map[string]bool{
	"-a": true, "-s": true, "-u": true, "-f": true, "-d": true, "--delete": true,
	"-m": true, "--annotate": true, "--sign": true, "--force": true,
}

// hasAnyFlag reports whether args contains one of flags.
func hasAnyFlag(args []string, flags map[string]bool) bool {
	for _, arg := range args {
		if flags[arg] {
			return true
		}
		if idx := strings.IndexByte(arg, '='); idx > 0 && flags[arg[:idx]] {
			return true
		}
	}
	return false
}

// gitRiskySubFlags are the subcommand options that let a subcommand that
// otherwise only reads run a program or write a file: --ext-diff loads an
// external differ, --open-files-in-pager runs a program over the matches, and
// --output writes the report to a path.
var gitRiskySubFlags = map[string]bool{
	"--ext-diff": true, "--open-files-in-pager": true, "--output": true,
}

// gitReadOnly reports whether a git invocation only reads. Mutating subcommands
// fall through to false and are scanned; their message arguments are blanked
// separately (messageArgs).
func gitReadOnly(args []string) bool {
	sub, rest, ok := gitSubcommand(args)
	if !ok {
		return false
	}
	// A global option before the subcommand can make a reading subcommand run a
	// program: -c sets configuration (core.pager, core.sshCommand, alias.*) and
	// --exec-path moves the helper lookup. Only the prefix counts — after the
	// subcommand, -c means something else entirely (`git grep -c halt` counts).
	for _, arg := range args[:len(args)-len(rest)-1] {
		if strings.HasPrefix(arg, "-c") || arg == "--exec-path" || strings.HasPrefix(arg, "--exec-path=") {
			return false
		}
	}
	if hasAnyFlag(rest, gitRiskySubFlags) {
		return false
	}
	switch sub {
	case "status", "diff", "log", "show", "rev-parse", "rev-list", "ls-files",
		"ls-tree", "describe", "blame", "annotate", "shortlog", "show-ref",
		"cat-file", "name-rev", "grep", "reflog", "whatchanged", "help",
		"version", "fetch", "check-ignore", "check-attr", "count-objects",
		"verify-commit", "verify-tag", "merge-base", "for-each-ref":
		return true
	case "branch":
		return !hasAnyFlag(rest, gitBranchListFlags)
	case "tag":
		return !hasAnyFlag(rest, gitTagListFlags)
	case "stash":
		return len(rest) == 0 || rest[0] == "list" || rest[0] == "show"
	case "remote":
		return len(rest) == 0 || rest[0] == "-v" || rest[0] == "show" || rest[0] == "get-url"
	case "worktree":
		return len(rest) == 0 || rest[0] == "list"
	case "submodule":
		return len(rest) == 0 || rest[0] == "status"
	}
	return false
}

// sedReadOnly reports whether sed only prints. `sed -i` edits in place, a script
// that writes a file (`w`, `W`), runs a command (`e`) or reads a file into the
// pattern space (`r`, `R`) does the same work a program would, and a script held
// in a file this gate does not read cannot be cleared at all — every one of them
// stays gated, so the whole invocation is scanned like any other segment.
func sedReadOnly(args []string) bool {
	var scripts []string
	explicit := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--in-place" || strings.HasPrefix(arg, "--in-place="):
			return false
		case len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg, 'i'):
			return false
		case strings.HasPrefix(arg, "-f") || strings.HasPrefix(arg, "--file"):
			// The script lives in a file: an unread script is not a read.
			return false
		case arg == "-e" || arg == "--expression":
			if i+1 < len(args) {
				scripts = append(scripts, args[i+1])
				explicit = true
				i++
			}
		case strings.HasPrefix(arg, "--expression="):
			scripts = append(scripts, strings.TrimPrefix(arg, "--expression="))
			explicit = true
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			scripts = append(scripts, arg[2:])
			explicit = true
		case arg == "-l" || arg == "--line-length":
			i++ // its value is a width, not a script
		}
	}
	if !explicit {
		// Without -e the first positional argument is the script.
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") && arg != "-" {
				continue
			}
			scripts = append(scripts, arg)
			break
		}
	}
	for _, script := range scripts {
		if sedScriptRunsCode(script) {
			return false
		}
	}
	return true
}

// sedScriptRunsCode reports whether a sed script can write a file or run a
// command. A "w" inside an address regex or a replacement is text, so the script
// is walked command by command: skip the address, read the command letter, and
// skip that command's own text.
func sedScriptRunsCode(script string) bool {
	for i := 0; i < len(script); {
		switch c := script[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' ||
			c == ';' || c == '{' || c == '}' || c == '!' || c == ',' || c == '=' ||
			c == '$' || (c >= '0' && c <= '9'):
			i++
		case c == '/':
			// An address regex: its text is data.
			next, ok := skipSedDelimited(script, i+1, '/')
			if !ok {
				return true // an unterminated field cannot be cleared
			}
			i = next
		case c == 's' || c == 'y':
			next, flags, ok := skipSedSubstitution(script, i)
			if !ok {
				return true
			}
			if strings.ContainsAny(flags, "we") {
				return true
			}
			i = next
		case c == 'w' || c == 'W' || c == 'e' || c == 'r' || c == 'R':
			return true
		default:
			// Any other command's argument runs to the next separator.
			for i < len(script) && script[i] != ';' && script[i] != '\n' {
				i++
			}
		}
	}
	return false
}

// skipSedDelimited returns the index just past the next unescaped delim found
// while scanning s from pos. ok is false when the field is unterminated.
func skipSedDelimited(s string, pos int, delim byte) (int, bool) {
	for i := pos; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case delim:
			return i + 1, true
		}
	}
	return 0, false
}

// skipSedSubstitution returns the index just past a substitution's replacement
// field, plus the flags that follow it. ok is false when the command is
// unterminated, which keeps the invocation on the scanned side.
func skipSedSubstitution(s string, start int) (int, string, bool) {
	if start+1 >= len(s) {
		return 0, "", false
	}
	delim := s[start+1]
	pos := start + 2
	for field := 0; field < 2; field++ {
		next, ok := skipSedDelimited(s, pos, delim)
		if !ok {
			return 0, "", false
		}
		pos = next
	}
	begin := pos
	for pos < len(s) && !strings.ContainsRune(" \t\r\n;}", rune(s[pos])) {
		pos++
	}
	return pos, s[begin:pos], true
}

// findReadOnly reports whether find only walks the tree. The traversal actions
// that run a command or delete what they match keep find on the scanned side.
func findReadOnly(args []string) bool {
	for _, arg := range args {
		switch {
		case arg == "-delete", arg == "-exec", arg == "-execdir", arg == "-ok", arg == "-okdir", arg == "-fls":
			return false
		case strings.HasPrefix(arg, "-fprint"):
			return false
		}
	}
	return true
}

// outputFlag reports whether args carries an output flag: one of the given long
// names, or a single-dash cluster containing the given short letter.
func outputFlag(args []string, short byte, longs ...string) bool {
	for _, arg := range args {
		for _, long := range longs {
			if arg == long || strings.HasPrefix(arg, long+"=") {
				return true
			}
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(short)) {
			return true
		}
	}
	return false
}

// sortReadOnly reports whether sort only reads. --compress-program runs a
// command and -o/--output writes a file, so neither invocation is a read.
func sortReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "--compress-program" || strings.HasPrefix(arg, "--compress-program=") {
			return false
		}
	}
	return !outputFlag(args, 'o', "--output")
}

// treeReadOnly reports whether tree only reads: -o/--output writes its listing
// to a file.
func treeReadOnly(args []string) bool { return !outputFlag(args, 'o', "--output") }

// dateReadOnly reports whether date only reads: -s/--set sets the system clock.
func dateReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "-s" || arg == "--set" || strings.HasPrefix(arg, "--set=") {
			return false
		}
	}
	return true
}

// curlSafeFlags are the curl options that keep an invocation a plain GET/HEAD.
var curlSafeFlags = map[string]bool{
	"-s": true, "--silent": true, "-S": true, "--show-error": true,
	"-I": true, "--head": true, "-i": true, "--include": true,
	"-L": true, "--location": true, "-f": true, "--fail": true,
	"-v": true, "--verbose": true, "--compressed": true,
	"-k": true, "--insecure": true, "-g": true, "--globoff": true,
	"-N": true, "--no-buffer": true, "--no-progress-meter": true,
	"-H": true, "--header": true, "-A": true, "--user-agent": true,
	"-m": true, "--max-time": true, "--connect-timeout": true,
	"--retry": true, "-4": true, "-6": true,
}

// curlValueFlags are the safe flags that consume a following value.
var curlValueFlags = map[string]bool{
	"-H": true, "--header": true, "-A": true, "--user-agent": true,
	"-m": true, "--max-time": true, "--connect-timeout": true, "--retry": true,
}

// curlClusterLetters are the single-letter curl flags that never take a value,
// so a cluster like `-sSL` or `-sI` is read as their conjunction.
const curlClusterLetters = "sSIiLfvkgN46"

// curlReadOnly reports whether curl is in a read-only form: GET or HEAD, with no
// request body, upload or method override. `curl -X POST -d ...` stays scanned.
func curlReadOnly(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue // a URL is data
		}
		if !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			// A cluster of value-less short flags: every letter must be safe.
			clusterOK := true
			for j := 1; j < len(arg); j++ {
				if !strings.ContainsRune(curlClusterLetters, rune(arg[j])) {
					clusterOK = false
					break
				}
			}
			if clusterOK {
				continue
			}
			return false
		}
		name := arg
		attached := false
		if idx := strings.IndexByte(arg, '='); idx > 0 {
			name = arg[:idx]
			attached = true
		}
		if !curlSafeFlags[name] {
			return false
		}
		if curlValueFlags[name] && !attached && i+1 < len(args) {
			i++
		}
	}
	return true
}

// alwaysReadOnly is the classifier for verbs that only ever read: their
// arguments are patterns, paths and text.
func alwaysReadOnly([]string) bool { return true }

// readOnlyInvocations maps an executing verb to a classifier deciding whether
// THIS invocation only reads. A verb that is absent is scanned exactly as
// before. Two families are absent on purpose: interpreters (sh, bash, python3,
// perl, ruby, node, awk) whose arguments ARE code, and wrappers (xargs, env,
// sudo, eval, exec, timeout, nice, nohup) whose arguments are another command.
var readOnlyInvocations = map[string]func([]string) bool{
	// Search: the pattern is data, however violent its vocabulary looks.
	"grep": alwaysReadOnly, "egrep": alwaysReadOnly, "fgrep": alwaysReadOnly,
	"rg": alwaysReadOnly, "ag": alwaysReadOnly, "ack": alwaysReadOnly,
	// Text filters and readers. Pagers are deliberately absent: `less` and
	// `more` run `!command` at their own prompt, so a file argument does not
	// make the invocation a read.
	"cat": alwaysReadOnly, "bat": alwaysReadOnly, "head": alwaysReadOnly,
	"tail": alwaysReadOnly,
	"nl":   alwaysReadOnly, "cut": alwaysReadOnly, "tr": alwaysReadOnly,
	"column": alwaysReadOnly, "fold": alwaysReadOnly, "paste": alwaysReadOnly,
	"uniq": alwaysReadOnly, "comm": alwaysReadOnly,
	"diff": alwaysReadOnly, "cmp": alwaysReadOnly, "wc": alwaysReadOnly,
	"od": alwaysReadOnly, "xxd": alwaysReadOnly, "strings": alwaysReadOnly,
	"jq": alwaysReadOnly, "yq": alwaysReadOnly, "printf": alwaysReadOnly,
	"echo": alwaysReadOnly,
	// Inspection.
	"ls": alwaysReadOnly, "dir": alwaysReadOnly,
	"stat": alwaysReadOnly, "file": alwaysReadOnly, "du": alwaysReadOnly,
	"df": alwaysReadOnly, "which": alwaysReadOnly, "whereis": alwaysReadOnly,
	"type": alwaysReadOnly, "basename": alwaysReadOnly, "dirname": alwaysReadOnly,
	"realpath": alwaysReadOnly, "id": alwaysReadOnly, "whoami": alwaysReadOnly,
	"hostname": alwaysReadOnly, "uname": alwaysReadOnly,
	"ps": alwaysReadOnly, "md5sum": alwaysReadOnly, "sha1sum": alwaysReadOnly,
	"sha256sum": alwaysReadOnly, "cksum": alwaysReadOnly,
	// Verbs with a read/write split: the classifier reads the arguments.
	"sed":  sedReadOnly,
	"find": findReadOnly,
	"git":  gitReadOnly,
	"curl": curlReadOnly,
	"sort": sortReadOnly,
	"tree": treeReadOnly,
	"date": dateReadOnly,
}
