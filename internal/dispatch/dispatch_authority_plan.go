package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	goatherd "github.com/goatherdai/goatherd-runtime"
	"github.com/goatherdai/yaah/internal/permissionplan"
)

// SchemaDispatchAuthority is the trace fact that records a dispatch's compiled
// authority: the permission plan the dispatch world was built to enforce, named
// by its digest. The world fact says which world ran; this fact says what that
// world promised to enforce, so a settlement that accepts the world's work can
// cite the exact declaration it is settling against.
const SchemaDispatchAuthority = "yaah.subagent.authority.v1"

// authorityForDispatch maps the configured dispatch authority onto one
// permissionplan.Authority declaration. It is a pure reading of the runtime's
// config: nothing here provisions, resolves, or lowers a path.
//
// The mapping and why each axis maps as it does:
//
//   - Writes come from the same grant set the container lowering uses
//     (dispatchGrants, so a role override wins), kept world-relative exactly as
//     configured. The plan is not the mount math — resolveContainerGrants still
//     turns these into absolute writable roots — it is the statement of what
//     those roots mean.
//   - Network mirrors agents.subagent.network, the vocabulary the plan already
//     speaks. An empty mode declares no clause: lowering keeps the documented
//     allow_all default, but the config did not declare egress, and a plan
//     records declarations, not defaults.
//   - Delta declares DeltaApply, because one declaration covers both monitors:
//     the same declaration that shapes the jail also settles its proposal. yaah's
//     settlement is an orchestrator-mediated whole-path apply — select and apply
//     write the manifest's post-images onto the target directly, and nothing
//     waits behind a second gate once the orchestrator invokes the verb. The
//     orchestrator invoking the verb IS the review, so the declared posture is
//     apply, and Compile assigns the clause the settlement monitor at commit.
//     DeltaReview is declined as a distinct posture rather than mapped from some
//     condition: yaah has no separate review-gate config knob, so declaring
//     "review" would claim a gate settlement does not enforce. Note the clause
//     depends on no jail either way — the settlement monitor lives in the parent
//     world, so an advisory dispatch declares it exactly as a jailed one does.
//   - Containment and Fallback come from agents.subagent.containment, which is
//     the axis that decides whether a jail monitor exists at all:
//     "jail"/"paddock" claims the jail outright. "auto" and the empty default
//     also claim it, because this function is only reached once the dispatcher
//     is building a confined world: the container dispatch path resolves its
//     authority after the world ladder chose the container, and the session-
//     container fallback question is only asked where a container world exists.
//     A declaration that reached here has a jail to be enforced by.
//     "off"/"none"/"advisory" claim no jail, and there the declaration's
//     grants and egress are accepted with FallbackEnforce rather than refused:
//     today's advisory lane lowers them onto the container anyway, so refusing
//     would turn a working advisory dispatch into a failed one. The degraded
//     posture is then recorded in the plan and covered by its digest instead of
//     being invisible. Any other value is an error: config validation already
//     refuses it, and a plan must never be built by guessing at a posture it
//     does not understand.
func authorityForDispatch(rt RetainedRuntime, role string) (permissionplan.Authority, error) {
	var a permissionplan.Authority

	grants := dispatchGrants(rt, role)
	a.Writes = make([]permissionplan.WriteClause, 0, len(grants))
	for _, grant := range grants {
		// The same canonical spelling resolveContainerGrants lowers, so the
		// plan and the mount shape can never disagree about what a grant says:
		// `src\foo` and `src/foo` are one clause and one mount.
		access := permissionplan.Access(strings.ToLower(strings.TrimSpace(grant.Access)))
		switch access {
		case permissionplan.AccessReadWrite, permissionplan.AccessReadOnly:
		default:
			return permissionplan.Authority{}, fmt.Errorf(
				"%s: unknown access %q is not one of: readwrite, readonly", grant.Path, grant.Access)
		}
		a.Writes = append(a.Writes, permissionplan.WriteClause{Root: canonicalGrantPath(grant.Path), Access: access})
	}

	network, err := networkPosture(rt.Network)
	if err != nil {
		return permissionplan.Authority{}, err
	}
	a.Network = network
	// The allowlist qualifies the posture rather than sitting beside it: two
	// dispatches whose brokers admit different hosts enforce different egress,
	// so they must not cite one declaration. Normalized exactly as provisioning
	// normalizes it, because the plan is a leaf package and must not learn how a
	// broker matches a host. Every other posture carries none — the lowering
	// drops hosts there, so the declaration does too.
	if network == permissionplan.NetworkBroker {
		a.NetworkHosts = NormalizeBrokerHosts(rt.NetworkHosts)
	}

	// One declaration, two monitors: the delta posture is declared here too, so
	// the plan's digest covers settlement as well as confinement. See the doc
	// comment above for why the posture is apply.
	a.Delta = permissionplan.DeltaApply

	containment, fallback, err := containmentPosture(rt.Containment)
	if err != nil {
		return permissionplan.Authority{}, err
	}
	a.Containment, a.Fallback = containment, fallback

	return a, nil
}

// networkPosture maps agents.subagent.network onto the plan's egress vocabulary.
// The accepted values and the refusal are lowerContainerNetwork's exactly, so a
// mode that lowers onto the container also compiles into a plan.
func networkPosture(mode string) (permissionplan.NetworkPosture, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return permissionplan.NetworkUnset, nil
	case "allow_all":
		return permissionplan.NetworkAllowAll, nil
	case "deny_all":
		return permissionplan.NetworkDenyAll, nil
	case "broker":
		return permissionplan.NetworkBroker, nil
	default:
		return permissionplan.NetworkUnset, fmt.Errorf(
			"agents.subagent.network %q is not one of: allow_all, deny_all, broker", mode)
	}
}

// containmentPosture maps agents.subagent.containment onto the plan's
// containment and fallback axes. See authorityForDispatch for why "auto" is a
// jail and why an advisory placement accepts its clauses instead of refusing
// them.
func containmentPosture(containment string) (permissionplan.Containment, permissionplan.Fallback, error) {
	switch strings.ToLower(strings.TrimSpace(containment)) {
	case "paddock", "jail":
		return permissionplan.ContainmentJail, permissionplan.FallbackRefuse, nil
	case "", "auto":
		return permissionplan.ContainmentJail, permissionplan.FallbackRefuse, nil
	case "off", "none", "advisory":
		return permissionplan.ContainmentAdvisory, permissionplan.FallbackEnforce, nil
	default:
		return permissionplan.ContainmentUnset, permissionplan.FallbackRefuse, fmt.Errorf(
			"agents.subagent.containment %q is not one of: off (or none), advisory, auto, paddock (or jail)", containment)
	}
}

// compileDispatchPlan compiles the dispatch's declared authority into its
// permission plan. Compile is the fail-closed gate: it refuses a declaration
// whose values it cannot lower, and it refuses one whose declared clause no
// monitor in the plan covers (permissionplan.ErrClauseUncovered). A dispatch
// world is never built from a plan that failed to compile.
func compileDispatchPlan(rt RetainedRuntime, role string) (permissionplan.PermissionPlan, error) {
	authority, err := authorityForDispatch(rt, role)
	if err != nil {
		return permissionplan.PermissionPlan{}, err
	}
	plan, err := permissionplan.Compile(authority)
	if err != nil {
		return permissionplan.PermissionPlan{}, fmt.Errorf("dispatch authority: %w", err)
	}
	return plan, nil
}

// recordAuthorityFact appends the compiled authority to the dispatch's scope,
// next to the world fact, so the jail's declaration is citable after the fact —
// by the settlement lane, which takes over the scope the world was sealed in. A
// failure to record is advisory, matching recordWorldFact: the run already
// happened.
func recordAuthorityFact(ctx context.Context, mgr *goatherd.ScopeManager, scopeID string, a dispatchAuthority) {
	if mgr == nil || scopeID == "" {
		return
	}
	scope, ok := mgr.Get(scopeID)
	if !ok {
		return
	}
	if _, err := scope.AppendFact(ctx, SchemaDispatchAuthority, "authority", authorityFactPayload(a)); err != nil {
		slog.Debug("authority fact: append failed", "scope", scope.ID(), "err", err)
	}
}

// authorityFactPayload is the recorded shape of a dispatch's compiled
// authority. It names the plan by digest — the citable identity — and records
// the lowered posture beside it so a reader need not re-derive the plan to see
// what the world was shaped to enforce. Kept separate so a test can pin the key
// set, as worldFactPayload's does.
func authorityFactPayload(a dispatchAuthority) map[string]any {
	return map[string]any{
		"plan_digest":     a.planIdentity(),
		"network_posture": a.network,
		"shaped":          a.shaped(),
		"writable_roots":  len(a.writableRoots),
	}
}
