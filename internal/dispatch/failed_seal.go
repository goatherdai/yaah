package dispatch

// Failed-seal proposals bridge the runtime's durable failed-seal artifacts to
// the host's proposal vocabulary. A failed seal keeps its worktree and its
// transcript in the runtime; this file exposes that handle as an open proposal
// with status "seal_failed", so the reaper spares it and the orchestrator can
// recover or discard it explicitly.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	goatherd "github.com/goatherdai/goatherd-runtime"
	goatherdsandbox "github.com/goatherdai/goatherd-runtime/sandbox"
	"github.com/goatherdai/yaah/internal/types"
)

// failedSealHasWorkspaceSeed reports whether a handle carries a usable
// workspace seed. ForkFromFailedSeal refuses a handle whose workspace state
// is empty — which is exactly what a digest-failure handle looks like after
// a restart, because the state values fact is deliberately never written on
// a failed hard path — so recover/continue would fail on it.
func failedSealHasWorkspaceSeed(h *goatherd.FailedSeal) bool {
	return h != nil && h.Workspace.Backend != ""
}

// sealFailedVerbs are the legal next verbs for a failed-seal proposal.
// Recovery needs both a captured conversation and a usable workspace seed:
// without a conversation the reseed fails on the empty-transcript gate, and
// without workspace state ForkFromFailedSeal refuses the fork. Only discard
// is advertised when either is missing.
func sealFailedVerbs(h *goatherd.FailedSeal) []string {
	if h == nil || len(h.Transcript) == 0 || !failedSealHasWorkspaceSeed(h) {
		return []string{"discard"}
	}
	// Both re-run verbs consume the artifact: the runtime resolves a source
	// artifact when a child citing it seals, so a non-consuming "continue"
	// would silently disappear on the next restart.
	return []string{"recover", "continue", "discard"}
}

// markSealFailedEnvelope stamps the typed recovery contract onto a failed-seal
// result at the lane, so callers that read the lane envelope directly (without
// the dispatch core's finalize step) see the same classification.
func markSealFailedEnvelope(env *proposalEnvelope, h *goatherd.FailedSeal) {
	env.Status = EnvelopeStatusSealFailed
	env.ProposalID = h.ArtifactID
	env.Next = sealFailedVerbs(h)
	env.FailureClass = FailureClassSealFailed
	if failedSealHasWorkspaceSeed(h) && len(h.Transcript) > 0 {
		env.Recovery = &ResultRecovery{
			Recommended: "inspect the failed-seal proposal with proposal show, then recover it or discard it explicitly; do not auto-redispatch",
		}
		env.Notes = trimResult("worktree and conversation retained for recovery; discard explicitly to abandon them")
	} else {
		env.Recovery = &ResultRecovery{
			Recommended: "inspect the failed-seal proposal with proposal show; it cannot be recovered (no usable workspace seed or conversation), so discard it explicitly",
		}
		env.Notes = trimResult("worktree retained; the artifact has no usable workspace seed or conversation, so only discard is available")
	}
}

func hasVerb(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// newFailedSealProposal turns a runtime handle into an open proposal. The
// handle's metadata and transcript use the same envelopes a successful seal
// writes, so a proposal looks the same before and after a restart.
func newFailedSealProposal(h *goatherd.FailedSeal, mgr *goatherd.ScopeManager, rt RetainedRuntime) (*retainedProposal, error) {
	if h == nil || h.ArtifactID == "" {
		return nil, fmt.Errorf("failed-seal handle has no artifact id")
	}
	var meta retainedProposalMeta
	if len(h.Metadata) > 0 {
		if err := json.Unmarshal(h.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("failed-seal artifact %s: decode metadata: %w", h.ArtifactID, err)
		}
	}
	var transcript []types.Message
	if len(h.Transcript) > 0 {
		if err := json.Unmarshal(h.Transcript, &transcript); err != nil {
			return nil, fmt.Errorf("failed-seal artifact %s: decode transcript: %w", h.ArtifactID, err)
		}
	}
	workdir := ""
	if scope, ok := mgr.Get(h.ScopeID); ok {
		if addressed, ok := scope.Sandbox().(goatherdsandbox.Addressed); ok {
			workdir = addressed.Address()
		}
	}
	p := &retainedProposal{
		id:                  h.ArtifactID,
		scopeID:             h.ScopeID,
		workdir:             workdir,
		result:              meta.Result,
		runErr:              meta.RunErr,
		parentID:            meta.ParentScopeID,
		mgr:                 mgr,
		transcript:          transcript,
		params:              meta.Params,
		timeout:             time.Duration(meta.TimeoutMS) * time.Millisecond,
		stopReason:          meta.StopReason,
		authorityPlanDigest: meta.AuthorityPlanDigest,
		world:               "worktree",
		repoPath:            rt.RepoPath,
		status:              "seal_failed",
		failedSeal:          h,
	}
	return p, nil
}

// registerFailedSealProposal adds a fresh handle to the live registry so the
// reaper spares its worktree and proposal list can show it immediately.
func registerFailedSealProposal(h *goatherd.FailedSeal, mgr *goatherd.ScopeManager, rt RetainedRuntime) error {
	if rt.Proposals == nil || h == nil {
		return nil
	}
	p, err := newFailedSealProposal(h, mgr, rt)
	if err != nil {
		return err
	}
	if existing, err := rt.Proposals.get(p.id); err == nil && existing.failedSeal != nil {
		return nil // already registered
	}
	rt.Proposals.add(p)
	return nil
}

// clearFailedSealArtifact disposes of a recoverable handle and its world. The
// runtime removes the handle automatically when a child citing it seals, so a
// "not found" result means the artifact was already resolved, not a failure.
// The registry entry is removed only after disposal succeeds, so a failed
// teardown leaves the handle inspectable and retryable.
func clearFailedSealArtifact(ctx context.Context, mgr *goatherd.ScopeManager, reg *ProposalRegistry, p *retainedProposal) error {
	if p == nil || p.failedSeal == nil {
		return nil
	}
	h := p.failedSeal
	if mgr != nil {
		if _, stillOpen := mgr.FailedSeal(h.ID); stillOpen {
			if err := mgr.DiscardFailedSeal(ctx, h.ID); err != nil {
				return fmt.Errorf("discard failed-seal artifact %s: %w", h.ID, err)
			}
		}
		// The runtime's discard deliberately leaves the sandbox untouched.
		// The host owns the worktree, so a consumed or discarded artifact
		// must tear its scope down too.
		if scope, ok := mgr.Get(h.ScopeID); ok {
			if parent, ok := mgr.Get(p.parentID); ok {
				if err := parent.Discard(scope); err != nil {
					return fmt.Errorf("discard failed-seal worktree %s: %w", h.ScopeID, err)
				}
			}
		}
	}
	if reg != nil {
		reg.remove(p.id)
	}
	return nil
}

// loadFailedSealProposals rehydrates handles after a restart. Handles whose
// transcript failed verification or whose worktree drifted are skipped: the
// runtime refuses to fork them, so surfacing them as recoverable would be a
// lie. They remain inspectable through the runtime API.
func loadFailedSealProposals(reg *ProposalRegistry, mgr *goatherd.ScopeManager, rt RetainedRuntime) []string {
	var warnings []string
	for _, h := range mgr.FailedSeals() {
		if h.Stale || h.TranscriptInvalid {
			warnings = append(warnings, fmt.Sprintf("failed-seal artifact %s is not recoverable (stale=%v, transcript_invalid=%v)", h.ArtifactID, h.Stale, h.TranscriptInvalid))
			continue
		}
		var meta retainedProposalMeta
		if len(h.Metadata) > 0 {
			_ = json.Unmarshal(h.Metadata, &meta)
		}
		// Ownership must be established before importing a handle from the
		// trace: a session mismatch, or an unknown session paired with a
		// different root scope, means this is another session's work.
		if rt.SessionID != "" && meta.SessionID != "" && meta.SessionID != rt.SessionID {
			continue
		}
		if rt.RootScopeID != "" && meta.SessionID == "" && meta.ParentScopeID != rt.RootScopeID {
			continue
		}
		if reg == nil {
			continue
		}
		p, err := newFailedSealProposal(h, mgr, rt)
		if err != nil {
			warnings = append(warnings, err.Error())
			slog.Warn("proposal rehydrate: failed-seal handle", "err", err)
			continue
		}
		// Rehydrated proposals belong to this session's root, not to the
		// parent id baked into a trace from an earlier session.
		if rt.RootScopeID != "" {
			p.parentID = rt.RootScopeID
		}
		reg.add(p)
	}
	return warnings
}
