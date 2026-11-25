package substrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PathValidator enforces workspace containment for file-accessing tools.
//
// A nil *PathValidator is safe — callers should check before calling ResolvePath
// and fall back to the legacy resolvePath behaviour.
type PathValidator struct {
	// WorkspaceRoot is the absolute, symlink-resolved root all file access
	// must stay within. Empty means "no restriction" (open access).
	WorkspaceRoot string

	// WorkDir is the directory relative paths resolve against and shell
	// commands execute in. Empty means the process working directory (the
	// legacy behaviour). An isolated sub-agent sets this to its worktree so
	// its file tools and shell address that checkout.
	WorkDir string

	// AllowHomeAccess enables ~ expansion.  When false, paths starting with
	// ~ are rejected outright.
	AllowHomeAccess bool

	// DenyPatterns are filepath.Match globs matched against the base
	// name of the resolved path and, when the path is inside the
	// workspace, against its slash-separated workspace-relative path
	// (so "config/*.secret" is expressible). Useful for blocking
	// sensitive files that might exist inside the workspace
	// (e.g. ".env", "*.pem").
	DenyPatterns []string

	// AskFn, when non-nil, converts a hard rejection into an interactive
	// approval prompt. It receives the offending path and a short reason
	// ("outside the workspace", deny pattern, home access); returning true
	// grants access for that path. A nil AskFn keeps hard-reject behaviour.
	AskFn func(path, reason string) bool

	// Harness, when non-nil, guards the harness's own configuration (role
	// files, skills, plans, yaah config/state) against agent writes. It is
	// checked only by ResolveWritePath and by the workspace write methods;
	// reads are never gated. The policy owns its own approval surface and
	// cache, so this is independent of AskFn and of the approval mode.
	Harness *HarnessPaths

	// ReadAllowRoots are directories OUTSIDE the workspace that read access is
	// granted to and write access is not (see AddReadAllowRoot). The
	// composition root registers the session's tool-result spill directory
	// here: a truncation notice tells the model to read the spilled output
	// back (internal/agent/context.RecoveryClause), and that promise is only
	// servable if the read tool can reach the file in a contained session.
	// Everything else outside the workspace stays refused, and ResolveWritePath
	// refuses these roots outright — the exemption is for reading a spool, not
	// for writing outside the workspace.
	ReadAllowRoots []string

	// mu guards approved.
	mu sync.Mutex
	// approved remembers granted exceptions keyed by path + reason class
	// so retries within a session do not re-prompt for the same access.
	approved map[string]bool
}

// PathRefusalError reports a path that the PathValidator refused. It carries
// the rejected path and, for containment refusals, the allowed workspace root
// so callers can render an actionable message instead of an opaque transport
// failure.
type PathRefusalError struct {
	// Path is the resolved path that was refused (or the raw input for
	// home-directory refusals that never resolved).
	Path string
	// Root is the workspace root the path had to stay within. Empty for
	// non-containment refusals such as deny patterns.
	Root string
	// Reason is a short human-readable explanation.
	Reason string
}

func (e *PathRefusalError) Error() string {
	if e.Root != "" {
		return fmt.Sprintf("path %q is outside the workspace root %q; choose a path inside the workspace", e.Path, e.Root)
	}
	if e.Reason != "" {
		return fmt.Sprintf("path %q is refused: %s", e.Path, e.Reason)
	}
	return fmt.Sprintf("path %q is refused", e.Path)
}

// NewPathValidator returns a PathValidator ready for use.
func NewPathValidator(workspaceRoot string, allowHome bool, denyPatterns []string) *PathValidator {
	// Resolve the workspace root up-front so every ResolvePath call is fast.
	if workspaceRoot != "" {
		abs, err := filepath.Abs(workspaceRoot)
		if err == nil {
			workspaceRoot = abs
		}
		if real, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
			workspaceRoot = real
		}
	}
	return &PathValidator{
		WorkspaceRoot:   workspaceRoot,
		AllowHomeAccess: allowHome,
		DenyPatterns:    denyPatterns,
	}
}

// WithWorkspaceRoot derives a validator for a different workspace root, used by
// isolated sub-agents whose checkout lives somewhere other than the session
// workspace (for example a git worktree).
//
// Policy is inherited — AllowHomeAccess, DenyPatterns, and AskFn carry over —
// but the approval cache is deliberately fresh, so an exception granted in one
// scope cannot silently widen another scope's access.
func (pv *PathValidator) WithWorkspaceRoot(root string) *PathValidator {
	if pv == nil {
		derived := NewPathValidator(root, false, nil)
		derived.WorkDir = derived.WorkspaceRoot
		return derived
	}
	derived := NewPathValidator(root, pv.AllowHomeAccess, pv.DenyPatterns)
	derived.AskFn = pv.AskFn
	derived.Harness = pv.Harness.WithWorkspaceRoot(root)
	// The read-only exemptions carry over: a sub-agent gets the same spill
	// directory its own truncation notices name, and a read-only grant cannot
	// widen a scope's writes.
	derived.ReadAllowRoots = append([]string(nil), pv.ReadAllowRoots...)
	derived.WorkDir = derived.WorkspaceRoot
	return derived
}

// AddReadAllowRoot grants read access to dir when it lies outside the
// workspace, without granting write access (ResolveWritePath still refuses it).
// The path is canonicalised the same way the workspace root is, so a symlinked
// or relative spill directory cannot smuggle in a different tree; a path that
// cannot be canonicalised is ignored rather than stored, because a root that
// cannot be compared is a root that silently grants nothing.
func (pv *PathValidator) AddReadAllowRoot(dir string) {
	if pv == nil || dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if real, err := resolveExistingAncestor(abs); err == nil {
		// The spill directory is created on the first spill, so it usually
		// does not exist yet; resolve the deepest existing ancestor instead
		// of dropping the grant.
		abs = real
	} else {
		return
	}
	for _, existing := range pv.ReadAllowRoots {
		if existing == abs {
			return
		}
	}
	pv.ReadAllowRoots = append(pv.ReadAllowRoots, abs)
}

// inReadAllowRoot reports whether the already symlink-resolved abs lies inside
// one of the read-only exemption roots.
func (pv *PathValidator) inReadAllowRoot(abs string) bool {
	for _, root := range pv.ReadAllowRoots {
		if root != "" && WithinRoot(abs, root) {
			return true
		}
	}
	return false
}

// ResolvePath validates and resolves a user-supplied file path.
//
// Steps (in order):
//  1. Expand ~ (if allowed) or reject it.
//  2. Convert to absolute path.
//  3. Resolve symlinks.
//  4. Check containment within WorkspaceRoot (if set).
//  5. Check deny patterns.
//
// Returns the resolved absolute path or an error suitable for display to
// the model.
func (pv *PathValidator) ResolvePath(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty path")
	}

	// ── 1. Home-directory expansion ──
	if input == "~" || strings.HasPrefix(input, "~/") {
		if !pv.AllowHomeAccess && !pv.ask("home:"+input, "home-directory access is not allowed in this session") {
			return "", &PathRefusalError{
				Path:   input,
				Reason: "home-directory access is not allowed in this session",
			}
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home directory: %w", err)
		}
		input = filepath.Join(home, input[1:]) // handles ~/foo and ~ alone
	}

	// ── 2. Absolute path ──
	// Relative inputs resolve against the session working directory when one is
	// configured, so a sub-agent running in an isolated checkout addresses that
	// checkout's files. With no WorkDir the process cwd applies (legacy).
	if pv.WorkDir != "" && !filepath.IsAbs(input) {
		input = filepath.Join(pv.WorkDir, input)
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", input, err)
	}

	// ── 3. Symlink resolution ──
	// Fails CLOSED: when no ancestor resolves, the path is rejected
	// rather than used unresolved (the old fallback let an unresolved
	// path through, which containment then validated against the
	// wrong tree). A not-yet-existing leaf is fine — its nearest
	// existing ancestor resolves and the tail is re-appended, which
	// also handles symlinks like macOS /var → /private/var.
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved, resolveErr := resolveExistingAncestor(abs)
		if resolveErr != nil {
			return "", fmt.Errorf("cannot resolve path %q: %w", abs, resolveErr)
		}
		realPath = resolved
	}

	// ── 4. Workspace containment ──
	// relInWorkspace is the slash-separated workspace-relative path when
	// the resolved path is inside the workspace; empty otherwise. Deny
	// patterns use it for path-segment globs (step 5).
	var relInWorkspace string
	if pv.WorkspaceRoot != "" {
		// Canonicalise the root one more time in case it changed since
		// construction.  Fast path if already canonicalised.
		root := pv.WorkspaceRoot

		rel, err := filepath.Rel(root, realPath)
		if err != nil {
			return "", fmt.Errorf("cannot compute relative path: %w", err)
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			// A read-only exemption outside the workspace (the session's spill
			// directory) is consulted before the refusal/approval path, so a
			// grant that needs no prompt never consumes one. Deny patterns
			// below still apply: operator policy outranks the exemption.
			if !pv.inReadAllowRoot(realPath) && !pv.ask(realPath, fmt.Sprintf("outside the workspace (%s)", root)) {
				return "", &PathRefusalError{Path: realPath, Root: root, Reason: "outside the workspace"}
			}
		} else {
			relInWorkspace = filepath.ToSlash(rel)
		}
	}

	// ── 5. Deny patterns ──
	// Every pattern is tried against the basename and, when the path is
	// inside the workspace, against its workspace-relative path so both
	// "*.pem" and "config/*.secret" styles work. A malformed pattern
	// fails CLOSED (deny): discarding the error would silently disable
	// the protection the operator configured.
	base := filepath.Base(realPath)
	for _, pattern := range pv.DenyPatterns {
		baseMatched, baseErr := filepath.Match(pattern, base)
		relMatched := false
		if relInWorkspace != "" {
			relMatched, _ = filepath.Match(pattern, relInWorkspace)
		}
		if baseErr == nil && !baseMatched && !relMatched {
			continue
		}
		reason := fmt.Sprintf("matches deny pattern %q", pattern)
		if baseErr != nil {
			reason = fmt.Sprintf("deny pattern %q is invalid (%v)", pattern, baseErr)
		}
		if pv.ask("deny:"+realPath, reason) {
			continue
		}
		return "", &PathRefusalError{
			Path:   realPath,
			Reason: fmt.Sprintf("access is forbidden by deny pattern %q", pattern),
		}
	}

	return realPath, nil
}

// ResolveWritePath is ResolvePath plus the harness-configuration guard: every
// mutating file tool uses it, so a write to a role file, skill, plan, or yaah
// config/state is refused (global) or requires approval (workspace) before the
// tool touches anything. Reads keep using ResolvePath and are never gated —
// inspecting a role file is how the agent explains itself.
//
// The guard runs after symlink resolution and containment, so `..` traversal
// and a symlink pointing at a role file cannot dodge it.
func (pv *PathValidator) ResolveWritePath(input string) (string, error) {
	abs, err := pv.ResolvePath(input)
	if err != nil {
		return "", err
	}
	if err := pv.refuseReadOnlyRootWrite(abs); err != nil {
		return "", err
	}
	if err := pv.CheckHarnessWrite(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// refuseReadOnlyRootWrite refuses a write into a read-only exemption root when
// the workspace does not already contain it. The exemption exists so a spooled
// result can be READ back; honouring it for writes would turn a display aid
// into a general write path outside the workspace.
func (pv *PathValidator) refuseReadOnlyRootWrite(abs string) error {
	if pv == nil || pv.WorkspaceRoot == "" || !pv.inReadAllowRoot(abs) {
		return nil
	}
	if WithinRoot(abs, pv.WorkspaceRoot) {
		return nil
	}
	return &PathRefusalError{
		Path:   abs,
		Reason: "read-only spool directory outside the workspace: reads are allowed, writes are not",
	}
}

// CheckHarnessWrite applies the harness-path policy to an already-resolved
// absolute path. Direct writers that do not go through the workspace (the role
// tool, apply_out) call it themselves.
func (pv *PathValidator) CheckHarnessWrite(abs string) error {
	if pv == nil || pv.Harness == nil {
		return nil
	}
	return pv.Harness.CheckWrite(abs)
}

// resolveExistingAncestor resolves the deepest existing ancestor of abs
// via EvalSymlinks and re-appends the unresolved tail, so paths whose
// leaf does not exist yet still get their ancestor symlinks resolved.
// Returns an error when nothing along the path resolves — callers must
// reject such paths rather than use them unresolved. A tail component
// that exists as a symlink EvalSymlinks cannot resolve is dangling: it
// is rejected rather than re-appended, because a later write would
// follow it outside the workspace.
func resolveExistingAncestor(abs string) (string, error) {
	cur := abs
	var tail []string
	for i := 0; i < 64; i++ {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			out := real
			for j := len(tail) - 1; j >= 0; j-- {
				out = filepath.Join(out, tail[j])
			}
			return out, nil
		}
		// EvalSymlinks failed on cur. If cur itself exists, it is a
		// symlink whose target does not resolve — dangling. Re-appending
		// it would pass containment on the link path while the write
		// lands at the (missing, outside) target.
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("dangling symlink %q (target does not resolve)", cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
	return "", fmt.Errorf("no existing path component resolves")
}

// ask consults AskFn for an exception to a rejection, caching grants so
// the same path+reason is only prompted once per session. It returns
// false (hard reject) when no AskFn is configured.
func (pv *PathValidator) ask(path, reason string) bool {
	if pv.AskFn == nil {
		return false
	}
	key := path + "\x00" + reason
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if pv.approved[key] {
		return true
	}
	if !pv.AskFn(path, reason) {
		return false
	}
	if pv.approved == nil {
		pv.approved = make(map[string]bool)
	}
	pv.approved[key] = true
	return true
}

// PathValidatorSetter is implemented by file-accessing tools that need
// workspace containment.  The Registry calls SetPathValidator during
// Register so tools built via factories also receive the validator.
type PathValidatorSetter interface {
	SetPathValidator(pv *PathValidator)
}
