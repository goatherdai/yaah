package dispatch

// baseref.go resolves a caller-supplied base_ref — the commit a dispatch's
// world is seeded from and its delta is measured against — into the kernel
// workspace state the existing fork applies (yaah-witj increment B).
//
// Seeding a dispatch world is NOT a checkout. goatherd-runtime creates the
// world's worktree with `git worktree add --detach <path> <repo HEAD>` and
// ignores its SandboxSpec entirely, so there is no ref knob at creation and
// none can be added from here: the runtime and kernel modules are upstream
// dependencies. The reachable seam is the FORK SEED. yaah passes
// ForkOptions{Baseline, Seed} and the runtime applies the seed to the child's
// OWN sandbox — `reset --hard <head_sha>` then `read-tree --reset -u
// <tree_sha>` — before the fork record exists. base_ref is therefore
// implemented by SYNTHESIZING the workspace state of the requested commit and
// handing it to that fork. No kernel or runtime change.
//
// The synthesized state has to look exactly like a runtime `git` Capture:
// backend "git", Revision the commit sha, Data carrying head_sha and tree_sha.
// The tree is resolved here rather than left to the fork because kernel Apply
// refuses a state carrying neither tree_sha nor stash_sha, and a synthesized
// tree-less state would fail late, inside the fork, with no way to say why.

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/agent/subagent"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
)

// baseRefResolveTimeout bounds the whole base_ref resolution — the peel, the
// object check and the tree read — on the dispatch's own context, which
// carries the run's wall-clock budget. The same rationale as
// gitHeadReadTimeout: without a bound of its own, a git process parked on a
// held index.lock or a credential prompt would spend the dispatch's budget
// before the worktree exists.
const baseRefResolveTimeout = 10 * time.Second

// maxBaseRefBytes bounds the ref expression itself. A ref name git accepts is
// far shorter; the bound exists so a pathological input is refused by name
// rather than handed to argv.
const maxBaseRefBytes = 256

// baseRefNamePattern is the ALLOWLIST a base_ref must match before it reaches
// any git argument list.
//
// Allowlist, not denylist: the set of git revision syntaxes that reach outside
// a plain object lookup (`~`, `^`, `@{...}`, `<rev>:<path>`, `^!`, `:/text`)
// grows with git itself, so a denylist would have to be re-audited on every
// upgrade. Permit only the characters a branch, tag or abbreviated object name
// can consist of — letters, digits, `.`, `_` and `/`, with `-` allowed but not
// leading (a leading `-` must never be argv[0] of a subcommand, where it would
// be read as a flag). Everything else, including whitespace, `..`, `~`, `^`,
// `{`, `}` and `:`, is refused before the process starts.
// `+` is admitted because git admits it in a branch and tag name
// (`feature/c++` is a real spelling), so refusing it would refuse a legal ref;
// it carries no revision-expression meaning in git. Non-ASCII names stay
// excluded: they are legal in git but they are also the one class whose bytes
// depend on normalization (NFC vs NFD) and on the codepage of whoever is
// reading the error text, and admitting them would put a class into the
// allowlist that no case here can pin.
var baseRefNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/+-]*$`)

// Kernel workspace-state Data keys. The kernel's git Capture records these, and
// baseref.go is the first place in yaah that WRITES them (every other site only
// ever read a state an upstream capture produced), so the writer and the six
// readers name them through one const pair: an upstream rename then fails to
// compile here instead of quietly reading empty strings. They stay unexported
// because the layout is the kernel's, not ours to advertise.
const (
	workspaceStateHeadKey = "head_sha"
	workspaceStateTreeKey = "tree_sha"
)

// validateBaseRef refuses a base_ref that must never reach a git argument
// list. It is the first thing the resolver does, so every caller — including a
// future one that resolves a ref for a purpose other than seeding — inherits
// the gate.
func validateBaseRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("base_ref is empty; omit the parameter to seed from the parent's workspace, or name a branch, tag or commit")
	}
	if len(ref) > maxBaseRefBytes {
		return fmt.Errorf("base_ref is %d bytes; the maximum is %d — name a branch, tag or commit, not a revision expression", len(ref), maxBaseRefBytes)
	}
	if !baseRefNamePattern.MatchString(ref) {
		return fmt.Errorf("base_ref %q contains characters a ref name cannot hold, or that this parameter does not accept (a non-ASCII ref name is refused, as is whitespace and any revision syntax); name a branch, tag or commit (letters, digits, '.', '_', '/', '+' and non-leading '-'), not a revision expression such as HEAD~3, main@{1} or A..B", ref)
	}
	// A leading "/" makes the value a PATH, not a ref: the allowlist admits the
	// character because every refs/... name contains "/", so the position is
	// checked separately.
	if strings.HasPrefix(ref, "/") {
		return fmt.Errorf("base_ref %q looks like a filesystem path, not a ref; name a branch, tag or commit", ref)
	}
	// `..` is spelled with characters the allowlist permits and is a RANGE in
	// git revision syntax, so it is refused by name rather than by character.
	if strings.Contains(ref, "..") {
		return fmt.Errorf("base_ref %q is a revision range, not a ref; name the single commit this dispatch should start from", ref)
	}
	return nil
}

// baseRefCommit is a resolved base_ref: the ref as requested (trimmed), the
// commit it peels to, and the tree that identifies that commit's content.
type baseRefCommit struct {
	Ref    string
	Commit string
	Tree   string
}

// workspaceState is the kernel workspace state for the resolved commit — the
// shape a runtime `git` Capture records, synthesized so the existing fork
// applies it (see the file comment). It is the SEED and, through
// freshForkSeed, the BASELINE: the child starts at this commit and its delta
// is measured against it, so the recorded basis always describes its fork
// point.
func (c baseRefCommit) workspaceState() kerneltrace.WorkspaceState {
	return kerneltrace.WorkspaceState{
		Backend:  "git",
		Revision: c.Commit,
		Data: map[string]any{
			workspaceStateHeadKey: c.Commit,
			workspaceStateTreeKey: c.Tree,
		},
	}
}

// resolveBaseRef resolves ref to a commit and its tree in repoPath.
//
// repoPath is the PARENT's repository, always: a dispatch cannot fetch its own
// basis. `git fetch` writes the shared object store and FETCH_HEAD, so it is
// classified as a write and refused inside a dispatch worktree by design
// (yaah-witj / PR #425). A remote-tracking ref therefore resolves only when
// the parent repository already has it locally, and the not-fetched case gets
// its own message instead of a generic unknown-revision one.
//
// It resolves BEFORE anything is provisioned, which is the ordering the basis
// already had (retained.go): a failure here leaves nothing to retry against
// less than a partially created world.
func resolveBaseRef(ctx context.Context, repoPath, ref string) (baseRefCommit, error) {
	ref = strings.TrimSpace(ref)
	if err := validateBaseRef(ref); err != nil {
		return baseRefCommit{}, err
	}
	if repoPath == "" {
		return baseRefCommit{}, fmt.Errorf("base_ref %q cannot be resolved: this dispatch has no repository path to resolve it in", ref)
	}

	ctx, cancel := context.WithTimeout(ctx, baseRefResolveTimeout)
	defer cancel()

	// The `^{commit}` peel is required, not decoration: a bare `rev-parse
	// --verify <tag>` answers the TAG OBJECT sha for an annotated tag, and the
	// child's `read-tree` then fails on an object that is not a tree.
	commit, err := baseRefRevParse(ctx, repoPath, ref+"^{commit}")
	if err != nil {
		return baseRefCommit{}, baseRefResolveFailure(ctx, repoPath, ref, err)
	}

	// rev-parse can answer from a ref whose object is absent (a partial or
	// pruned clone), and the absence would then surface late, inside the
	// runtime's worktree creation or the seed apply. Ask now, so the error
	// names the ref and the repository instead of a git failure downstream.
	if err := baseRefObjectPresent(ctx, repoPath, commit); err != nil {
		return baseRefCommit{}, err
	}

	tree, err := baseRefRevParse(ctx, repoPath, commit+"^{tree}")
	if err != nil {
		if ctx.Err() != nil {
			return baseRefCommit{}, baseRefDeadlineError(ref, ctx.Err())
		}
		return baseRefCommit{}, fmt.Errorf("base_ref %q resolved to commit %s but its tree could not be read in %s: %w", ref, commit, repoPath, err)
	}
	return baseRefCommit{Ref: ref, Commit: commit, Tree: tree}, nil
}

// baseRefRevParse runs one `git -C repoPath rev-parse --verify --quiet <rev>`
// and returns the sha it prints. `--quiet` makes an unresolvable name an exit
// status rather than a stderr message, so a miss is classified here instead of
// guessed from output text.
//
// Every git invocation in this file is context-bound for the reason
// workspace_bootstrap.go states: a wedged host git must fail the dispatch's
// bound, not hang past it.
func baseRefRevParse(ctx context.Context, repoPath, rev string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--verify", "--quiet", rev).Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		// --quiet suppresses "unknown revision"; an empty answer is the same
		// miss, so it is reported as one rather than as an empty commit.
		return "", fmt.Errorf("rev-parse answered no object")
	}
	return sha, nil
}

// baseRefObjectPresent reports that commit is an object this repository
// actually holds.
func baseRefObjectPresent(ctx context.Context, repoPath, commit string) error {
	if _, err := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "-e", commit+"^{commit}").Output(); err != nil {
		if ctx.Err() != nil {
			return baseRefDeadlineError(commit, ctx.Err())
		}
		return fmt.Errorf("base_ref resolves to commit %s, but that object is not present in %s (a partial or pruned clone); fetch the commit into the parent repository first", commit, repoPath)
	}
	return nil
}

// baseRefDeadlineError renders a resolution that ran out of its own bound. It
// is separate from a miss: a timeout says nothing about whether the ref exists.
func baseRefDeadlineError(ref string, err error) error {
	return fmt.Errorf("base_ref %q: resolving the ref exceeded %s (%v); the repository's git may be wedged on a held index.lock or a credential prompt — retry once the repository answers normally", ref, baseRefResolveTimeout, err)
}

// baseRefResolveFailure renders a ref that did not resolve, separating the one
// case a caller can act on from the general miss: a remote-tracking ref this
// repository never fetched. A dispatch cannot fetch it (see resolveBaseRef),
// so "unknown revision" would send the caller looking for a typo instead of
// running `git fetch`.
func baseRefResolveFailure(ctx context.Context, repoPath, ref string, cause error) error {
	if ctx.Err() != nil {
		return baseRefDeadlineError(ref, ctx.Err())
	}
	if remote, ok := baseRefRemoteFor(ctx, repoPath, ref); ok {
		// A remote PREFIX match is not evidence the ref is merely unfetched: a
		// typo such as `origin/mian` matches remote `origin` exactly as an
		// unfetched `origin/main` does. Telling that caller to fetch would give
		// advice that can never work and would repeat on every retry, so the
		// "not fetched" claim is only made when this repository holds no
		// remote-tracking refs for that remote at all — the one case where
		// fetching is what actually helps.
		known, readable := baseRefRemoteNames(ctx, repoPath, remote)
		if readable && len(known) > 0 && !known[strings.TrimPrefix(ref, "refs/remotes/")] {
			return fmt.Errorf("base_ref %q looks like a remote-tracking ref of %q, but this repository holds no such ref: %q is either not fetched or has no branch of that name — check the spelling first (a dispatch cannot fetch for you: git fetch writes the shared object store, so it is refused inside a dispatch worktree), then fetch it in the parent repository", ref, remote, remote)
		}
		return fmt.Errorf("base_ref %q is a remote-tracking ref of %q that this repository has not fetched locally; a dispatch cannot fetch it for you (git fetch writes the shared object store, so it is refused inside a dispatch worktree) — fetch it in the parent repository first, or name a ref that is already local", ref, remote)
	}
	return fmt.Errorf("base_ref %q does not resolve to a commit in %s; name a branch, tag or commit that exists in the parent repository (an abbreviated object name works when it is unambiguous)", ref, repoPath)
}

// baseRefRemoteFor reports the configured remote a ref is shaped like a
// remote-tracking ref of, so an unfetched `origin/main` is told apart from a
// typo. A repository with no remotes, or a ref that names none of them, is not
// this case.
func baseRefRemoteFor(ctx context.Context, repoPath, ref string) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "remote").Output()
	if err != nil {
		return "", false
	}
	trimmed := strings.TrimPrefix(ref, "refs/remotes/")
	for _, line := range strings.Split(string(out), "\n") {
		remote := strings.TrimSpace(line)
		if remote == "" {
			continue
		}
		if trimmed == remote || strings.HasPrefix(trimmed, remote+"/") {
			return remote, true
		}
	}
	return "", false
}

// baseRefRemoteNames lists the remote-tracking ref names this repository holds
// for remote (each without the leading "refs/remotes/"), so a failure can tell
// "never fetched" from "no such branch on that remote". readable is false when
// git could not answer at all — the caller then falls back to the unfetched
// message rather than inventing a claim it cannot support.
func baseRefRemoteNames(ctx context.Context, repoPath, remote string) (map[string]bool, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "for-each-ref", "--format=%(refname)", "refs/remotes/"+remote+"/").Output()
	if err != nil {
		return nil, false
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names[strings.TrimPrefix(name, "refs/remotes/")] = true
	}
	return names, true
}

// baseRefWorldRefusal reports why a base_ref dispatch cannot run in the RESOLVED
// world, or "" when that world can carry the caller's basis.
//
// It is pure and separate from startRetainedRun so the rule is table-testable
// without a repository or a world: the refusal keys on the resolved world, never
// on the requested one, because an `auto` request legitimately degrades
// container→worktree (dispatchWorldPreflightFailure) and refusing on the
// request would punish exactly that dispatch. A container world is seeded from a
// session snapshot or a host-repo copy-in, not from a git ref, so there is no
// state to synthesize for it. The report lane is NOT refused: seeding a report
// world at a ref — a reviewer dispatched against a branch — is genuinely useful,
// and the worktree mechanism is identical.
func baseRefWorldRefusal(baseRef string, resolved subagent.WorldKind) string {
	if strings.TrimSpace(baseRef) == "" {
		return ""
	}
	if resolved == subagent.WorldContainer {
		return fmt.Sprintf("base_ref %q is worktree-lane only; the container lane is seeded from a session snapshot or a host-repo copy-in, not from a git ref", strings.TrimSpace(baseRef))
	}
	return ""
}
