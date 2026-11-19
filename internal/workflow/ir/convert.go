package ir

import (
	"slices"

	"github.com/goatherdai/yaah/internal/workflow/spec"
)

// The resolved enums that mirror the authoring surface one-to-one are type
// aliases in graph.go, so no conversion is needed. Resolve still accepts
// hand-built documents, so these two helpers keep the decoder's "unknown
// enum" guard in one place instead of in a per-enum converter.
func validateEnum[T ~string](v T) bool {
	var zero T
	return slices.Contains(spec.EnumValues(zero), string(v))
}

// checkEnum reports an unknown enum value at the given authoring path.
// allowEmpty covers the fields whose zero value means "undeclared" rather
// than an invalid value.
func checkEnum[T ~string](path, field string, v T, allowEmpty bool) error {
	if string(v) == "" && allowEmpty {
		return nil
	}
	if !validateEnum(v) {
		return resolveErrf(path+"."+field, "unknown %s %q", field, string(v))
	}
	return nil
}

// convertSelectPolicy converts the selection half of the shared authoring
// `policy` key. A settlement policy is not a selection policy and is
// rejected rather than dropped: WF014 names the field the resolver dropped.
func convertSelectPolicy(p spec.Policy) (SelectPolicy, error) {
	switch p {
	case spec.PolicyJudge:
		return SelectJudge, nil
	case spec.PolicyFirstSuccess:
		return SelectFirstSuccess, nil
	case spec.PolicySmallestDiff:
		return SelectSmallestDiff, nil
	}
	return "", resolveErrf("", "unconvertible select policy %q", p)
}

// convertSettlePolicy converts the settlement half of the shared authoring
// `policy` key, rejecting a value from the selection half.
func convertSettlePolicy(p spec.Policy) (SettlePolicy, error) {
	switch p {
	case spec.PolicyPathDisjoint:
		return SettlePathDisjoint, nil
	case spec.PolicyThreeWay:
		return SettleThreeWay, nil
	}
	return "", resolveErrf("", "unconvertible settle policy %q", p)
}

// convertGrants maps the authoring grant slice to the resolved shape. The
// access enum is now an alias, so only the enclosing struct differs.
func convertGrants(path string, gs []spec.Grant) ([]Grant, error) {
	if gs == nil {
		return nil, nil
	}
	out := make([]Grant, 0, len(gs))
	for _, g := range gs {
		if err := checkEnum(path+".grants", "access", g.Access, false); err != nil {
			return nil, err
		}
		out = append(out, Grant{Path: g.Path, Access: GrantAccess(g.Access)})
	}
	return out, nil
}
