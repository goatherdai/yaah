package dispatch

// The sealed output is the durable record for a worktree proposal. Its
// Snapshot carries the captured conversation, its Metadata carries the rest of
// what a restart needs to show, continue, and settle the proposal, and its
// retained diff is the review text. There is no sidecar: the trace plus the
// named-blob store are the source of truth.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	goatherd "github.com/goatherdai/goatherd-runtime"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
)

// maxDurableTranscriptBytes bounds the transcript carried in the sealed
// output's execution snapshot. A larger transcript is not truncated — a
// partial message sequence can orphan tool calls — it is omitted, leaving the
// proposal settleable but not conversation-continuable after a restart.
const maxDurableTranscriptBytes = 8 << 20 // 8 MiB

// retainedProposalMeta is the proposal state carried in SealOptions.Metadata.
// The conversation rides separately in SealOptions.Snapshot and the diff is
// retained by the seal itself, so neither is duplicated here.
type retainedProposalMeta struct {
	ID            string              `json:"id,omitempty"`
	SessionID     string              `json:"session_id,omitempty"`
	ParentScopeID string              `json:"parent_scope_id,omitempty"`
	Result        string              `json:"result,omitempty"`
	RunErr        string              `json:"run_err,omitempty"`
	StopReason    string              `json:"stop_reason,omitempty"`
	Params        jobs.SubAgentParams `json:"params,omitempty"`
	TimeoutMS     int64               `json:"timeout_ms,omitempty"`
	// TranscriptOmitted records that the conversation exceeded the snapshot
	// bound and was dropped rather than truncated.
	TranscriptOmitted bool `json:"transcript_omitted,omitempty"`
	// AuthorityPlanDigest is the digest of the permission plan the worktree
	// world was built to enforce — the declaration identity the seal cites and
	// the settlement then records it is the commit-time monitor for. It is the
	// worktree lane's counterpart to the container meta.json's field of the same
	// name: persisted with the seal so a proposal rehydrated in a later process
	// still cites the declaration it is being settled against instead of a
	// digest this process cannot know.
	AuthorityPlanDigest string `json:"authority_plan_digest,omitempty"`
	// Basis is the RESOLVED workspace state this dispatch's delta is measured
	// against (yaah-witj increment A): durable because a rehydrated or resumed
	// proposal must still be able to say what its work started from, and
	// resolved because the request parameters say what was ASKED for. Absent
	// means no basis was recorded, never "the basis is empty" — so a seal that
	// wrote nothing here reports no basis rather than an empty one. Its readers
	// are BOTH rehydration paths, and each hands it to
	// retainedProposal.sealBasisRecord: the worktree/container rehydration in
	// proposal_rehydrate.go, and newFailedSealProposal in failed_seal.go (where
	// it is frequently the only surviving copy, because a digest-failure
	// artifact records no workspace state at all). Every envelope prefers the
	// live state and falls back to this when the state is gone.
	Basis *proposalBasisRecord `json:"basis,omitempty"`
}

// persistProposalTranscript writes the captured conversation into a durable
// attempt transcript before the seal. A failed seal can then keep the handle's
// conversation — the old Snapshot path lost it whenever sealing failed.
// Oversized or unencodable transcripts are omitted and marked, never
// truncated: a partial message sequence can orphan tool calls.
func persistProposalTranscript(ctx context.Context, mgr *goatherd.ScopeManager, attemptID, scopeID string, transcript []types.Message) (string, bool, error) {
	var data []byte
	omitted := false
	if len(transcript) > 0 {
		encoded, err := json.Marshal(transcript)
		switch {
		case err != nil:
			omitted = true
			slog.Warn("proposal seal: transcript is not JSON-encodable; recording an empty transcript", "err", err)
		case len(encoded) <= maxDurableTranscriptBytes:
			data = encoded
		default:
			omitted = true
		}
	}
	id, err := mgr.PersistAttemptTranscript(ctx, attemptID, scopeID, data)
	if err != nil {
		return "", omitted, fmt.Errorf("persist attempt transcript: %w", err)
	}
	return id, omitted, nil
}

// proposalSealOptions builds the seal payload for a worktree proposal. The
// conversation rides in its own durable transcript fact, cited by TranscriptID;
// everything else becomes opaque metadata.
func proposalSealOptions(meta retainedProposalMeta, transcriptID string) (goatherd.SealOptions, error) {
	// The seed is recomputed at continuation time; persisting it would double
	// the transcript and the workdir is per-run. A source link describes how
	// this proposal was originally threaded, not how to continue it.
	//
	// This strip list is for TRANSIENT request state only. A parameter that
	// describes what the work was built from belongs in the resolved basis
	// record (retainedProposalMeta.Basis), not here: anything added to
	// SubAgentParams and silently stripped would leave the durable record
	// claiming a basis the dispatch never used (yaah-witj increment A).
	//
	// BaseRef is stripped for exactly that reason and NOT because it is
	// uninteresting: it is a REQUEST field, and what it names MOVES. The
	// durable record of where this work started from is Basis above, which the
	// dispatch stamped with the commit and tree the ref resolved to at
	// dispatch time. Keeping the ref name too would let a resumed proposal,
	// and a continuation built from these params (ContinueRetainedRun copies
	// them wholesale), carry a name that now points somewhere else — the
	// phantom-drift class yaah-9m0c closed, reintroduced as a stale label.
	params := meta.Params
	params.SeedMessages = nil
	params.Workdir = ""
	params.SeedFrom = ""
	params.SeedMode = ""
	params.BaseRef = ""
	meta.Params = params

	opts := goatherd.SealOptions{
		AttemptID:    meta.ID,
		TranscriptID: transcriptID,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return goatherd.SealOptions{}, fmt.Errorf("marshal proposal metadata: %w", err)
	}
	opts.Metadata = data
	return opts, nil
}

// proposalMetaFromOutput reads what proposalSealOptions wrote. Missing
// metadata means the output was sealed before durable proposal state existed;
// that is reported, not guessed.
func proposalMetaFromOutput(out *goatherd.RetainedOutput) (retainedProposalMeta, []types.Message, error) {
	var meta retainedProposalMeta
	raw := out.Metadata()
	if len(raw) == 0 {
		return meta, nil, fmt.Errorf("sealed output has no proposal metadata (sealed before durable proposal state)")
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, nil, fmt.Errorf("decode proposal metadata: %w", err)
	}
	var transcript []types.Message
	if snapshot := out.Snapshot(); len(snapshot) > 0 {
		if err := json.Unmarshal(snapshot, &transcript); err != nil {
			// Metadata is enough to list and settle the proposal; only the
			// conversation seed is lost. Degrade instead of dropping the
			// whole proposal.
			slog.Warn("proposal rehydrate: transcript snapshot is corrupt; proposal stays settleable but not conversation-continuable",
				"output", out.ID(), "err", err)
			meta.TranscriptOmitted = true
			return meta, nil, nil
		}
	}
	return meta, transcript, nil
}
