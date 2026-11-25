// subagent_aliases.go re-exports the sub-agent I/O contract and
// context-key helpers from internal/jobs so existing consumers keep
// compiling while the implementation lives in the jobs package.
package tools

import (
	"context"

	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/types"
)

type (
	TaskRunner     = jobs.TaskRunner
	SubAgentParams = jobs.SubAgentParams
	Escalation     = jobs.Escalation
	SubAgentWorld  = jobs.SubAgentWorld
)

const (
	EscalationBlocker = jobs.EscalationBlocker
)

// ErrStuckChild stays a variable alias so errors.Is identity is preserved.
var ErrStuckChild = jobs.ErrStuckChild

// ErrDispatchStartup is the dispatch-startup counterpart of ErrStuckChild:
// the world was provisioned but the child never heartbeat, so the dispatch
// never started rather than hanging mid-run.
var ErrDispatchStartup = jobs.ErrDispatchStartup

// ErrScopeExhausted is a variable alias of jobs.ErrScopeExhausted (same
// rule as above: variable alias so errors.Is identity is preserved).
var ErrScopeExhausted = jobs.ErrScopeExhausted

func ParseSubAgentOutput(output string, runErr error) *jobs.SubAgentOutput {
	return jobs.ParseSubAgentOutput(output, runErr)
}

func WithSubAgentModelPtr(ctx context.Context, ptr *string) context.Context {
	return jobs.WithSubAgentModelPtr(ctx, ptr)
}

func WriteSubAgentModel(ctx context.Context, model string) {
	jobs.WriteSubAgentModel(ctx, model)
}

func WithSubAgentStartNotifier(ctx context.Context, fn func(model string)) context.Context {
	return jobs.WithSubAgentStartNotifier(ctx, fn)
}

func NotifySubAgentStart(ctx context.Context, model string) {
	jobs.NotifySubAgentStart(ctx, model)
}

func WithSubAgentWorldPtr(ctx context.Context, ptr *SubAgentWorld) context.Context {
	return jobs.WithSubAgentWorldPtr(ctx, ptr)
}

func WriteSubAgentWorld(ctx context.Context, w SubAgentWorld) {
	jobs.WriteSubAgentWorld(ctx, w)
}

func WithSubAgentUsage(ctx context.Context, usage *types.Usage) context.Context {
	return jobs.WithSubAgentUsage(ctx, usage)
}

func AddSubAgentUsage(ctx context.Context, delta types.Usage) {
	jobs.AddSubAgentUsage(ctx, delta)
}

// CaptureSubAgentTranscript reports the sub-agent's final conversation through
// the transcript sink in ctx, if one was installed.
func CaptureSubAgentTranscript(ctx context.Context, messages []types.Message) {
	jobs.CaptureSubAgentTranscript(ctx, messages)
}

// SubAgentStop is the runner's typed stop outcome (aliased from jobs).
type SubAgentStop = jobs.SubAgentStop

// CaptureSubAgentStop reports the run's typed stop outcome through the sink in
// ctx, if one was installed.
func CaptureSubAgentStop(ctx context.Context, stop SubAgentStop) {
	jobs.CaptureSubAgentStop(ctx, stop)
}

// SubAgentDenial is one supervisor-guard refusal a run recorded (aliased from
// jobs), so the dispatch envelope can report what the child was not allowed to
// do instead of leaving it in the trace alone.
type SubAgentDenial = jobs.SubAgentDenial

// WithSubAgentDenials installs the per-run sink the runner reports its
// supervisor-guard refusals through.
func WithSubAgentDenials(ctx context.Context, ptr *[]SubAgentDenial) context.Context {
	return jobs.WithSubAgentDenials(ctx, ptr)
}

// CaptureSubAgentDenials reports the run's guard refusals through the sink in
// ctx, if one was installed.
func CaptureSubAgentDenials(ctx context.Context, denials []SubAgentDenial) {
	jobs.CaptureSubAgentDenials(ctx, denials)
}

// SubAgentConfinement is the confinement one run actually had (aliased from
// jobs), so the runner can report the jail it established without importing the
// dispatch lane's vocabulary.
type SubAgentConfinement = jobs.SubAgentConfinement

// CaptureSubAgentConfinement reports the run's jail through the sink in ctx, if
// one was installed.
func CaptureSubAgentConfinement(ctx context.Context, conf SubAgentConfinement) {
	jobs.CaptureSubAgentConfinement(ctx, conf)
}

func WithSubAgentReadyNotifier(ctx context.Context, fn func()) context.Context {
	return jobs.WithSubAgentReadyNotifier(ctx, fn)
}

func NotifySubAgentReady(ctx context.Context) {
	jobs.NotifySubAgentReady(ctx)
}

func WithSubAgentHeartbeat(ctx context.Context, hb chan struct{}) context.Context {
	return jobs.WithSubAgentHeartbeat(ctx, hb)
}

func SendHeartbeat(ctx context.Context) {
	jobs.SendHeartbeat(ctx)
}

// SubAgentIntervention is one supervision intervention the settling run was
// subject to — a decision or a rules action, taken or dropped, in tick order
// (aliased from the supervision vocabulary so the envelope and the batch name
// one record shape; yaah-vaxv).
type SubAgentIntervention = supervisionapi.InterventionRecord

// WithSubAgentInterventions installs the per-run sink the runner reports its
// scope's intervention records through.
func WithSubAgentInterventions(ctx context.Context, ptr *[]SubAgentIntervention) context.Context {
	return jobs.WithSubAgentInterventions(ctx, ptr)
}

// CaptureSubAgentInterventions reports the run's intervention records through
// the sink in ctx, if one was installed.
func CaptureSubAgentInterventions(ctx context.Context, records []SubAgentIntervention) {
	jobs.CaptureSubAgentInterventions(ctx, records)
}
