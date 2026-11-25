package tools

import (
	"fmt"
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
		// Unclassifiable line: fail closed with exactly the old substring scan.
		if pattern, bad := scanDenyPatterns(cmd); bad {
			return shellDenial{Pattern: pattern}, true
		}
		return shellDenial{}, false
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
	if pattern, bad := scanDenyPatterns(redactMessageArgs(seg, verb, seg.toks[verbIdx+1:])); bad {
		return shellDenial{Pattern: pattern, Verb: verb, Segment: clipSegment(seg.raw)}, true
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
				// Cluster form: `-m"..."`, `-mseparate-word`. The flag itself
				// carries no pattern, so the whole token is blanked.
				blank(tok)
			}
		}
	}
	return string(redacted)
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

// gitReadOnly reports whether a git invocation only reads. Mutating subcommands
// fall through to false and are scanned; their message arguments are blanked
// separately (messageArgs).
func gitReadOnly(args []string) bool {
	sub, rest, ok := gitSubcommand(args)
	if !ok {
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

// sedReadOnly reports whether sed only prints. `sed -i` edits in place and stays
// gated, so its script is scanned like any other argument.
func sedReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			return false
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg, 'i') {
			return false
		}
	}
	return true
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
	// Text filters and readers.
	"cat": alwaysReadOnly, "bat": alwaysReadOnly, "head": alwaysReadOnly,
	"tail": alwaysReadOnly, "less": alwaysReadOnly, "more": alwaysReadOnly,
	"nl": alwaysReadOnly, "cut": alwaysReadOnly, "tr": alwaysReadOnly,
	"column": alwaysReadOnly, "fold": alwaysReadOnly, "paste": alwaysReadOnly,
	"uniq": alwaysReadOnly, "sort": alwaysReadOnly, "comm": alwaysReadOnly,
	"diff": alwaysReadOnly, "cmp": alwaysReadOnly, "wc": alwaysReadOnly,
	"od": alwaysReadOnly, "xxd": alwaysReadOnly, "strings": alwaysReadOnly,
	"jq": alwaysReadOnly, "yq": alwaysReadOnly, "printf": alwaysReadOnly,
	"echo": alwaysReadOnly,
	// Inspection.
	"ls": alwaysReadOnly, "dir": alwaysReadOnly, "tree": alwaysReadOnly,
	"stat": alwaysReadOnly, "file": alwaysReadOnly, "du": alwaysReadOnly,
	"df": alwaysReadOnly, "which": alwaysReadOnly, "whereis": alwaysReadOnly,
	"type": alwaysReadOnly, "basename": alwaysReadOnly, "dirname": alwaysReadOnly,
	"realpath": alwaysReadOnly, "id": alwaysReadOnly, "whoami": alwaysReadOnly,
	"hostname": alwaysReadOnly, "uname": alwaysReadOnly, "date": alwaysReadOnly,
	"ps": alwaysReadOnly, "md5sum": alwaysReadOnly, "sha1sum": alwaysReadOnly,
	"sha256sum": alwaysReadOnly, "cksum": alwaysReadOnly,
	// Verbs with a read/write split: the classifier reads the arguments.
	"sed":  sedReadOnly,
	"find": findReadOnly,
	"git":  gitReadOnly,
	"curl": curlReadOnly,
}
