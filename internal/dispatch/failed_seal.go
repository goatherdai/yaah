package dispatch

// Failed-seal proposals bridge the runtime's durable failed-seal artifacts to
// the host's proposal vocabulary. A failed seal keeps its worktree and its
// transcript in the runtime; this file exposes that handle as an open proposal
// with status "seal_failed", so the reaper spares it and the orchestrator can
// recover or discard it explicitly.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	goatherd "github.com/goatherdai/goatherd-runtime"
	goatherdsandbox "github.com/goatherdai/goatherd-runtime/sandbox"
	"github.com/goatherdai/yaah/internal/types"
)

// sealFailedVerbs are the legal next verbs for a failed-seal proposal: the
// work can be reseeded from its artifact, or explicitly abandoned. Settlement
// verbs are not legal — there is no sealed output to settle.
func sealFailedVerbs() []string { return []string{"recover", "continue", "discard"} }

// markSealFailedEnvelope stamps the typed recovery contract onto a failed-seal
// result at the lane, so callers that read the lane envelope directly (without
// the dispatch core's finalize step) see the same classification.
func markSealFailedEnvelope(env *proposalEnvelope, artifactID string) {
	env.Status = EnvelopeStatusSealFailed
	env.ProposalID = artifactID
	env.Next = sealFailedVerbs()
	env.FailureClass = FailureClassSealFailed
	env.Recovery = &ResultRecovery{
		Recommended: "inspect the failed-seal proposal with proposal show, then recover it (or discard it explicitly); do not auto-redispatch",
	}
	env.Notes = trimResult("worktree and conversation retained for recovery; discard explicitly to abandon them")
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
		if rt.SessionID != "" {
			var meta retainedProposalMeta
			if len(h.Metadata) > 0 {
				_ = json.Unmarshal(h.Metadata, &meta)
			}
			if meta.SessionID != "" && meta.SessionID != rt.SessionID {
				continue
			}
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
		reg.add(p)
	}
	return warnings
}
