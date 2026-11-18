package validate

import (
	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/spec"
)

// Structural runs the structural family (WF002–WF005) over a resolved
// graph and returns every finding. Rules run in code order and each rule
// reports in document order, so the slice is deterministic for a given
// graph: it can be compared, counted, or shown verbatim without sorting
// first. A nil graph has nothing to check and yields no findings.
//
// Later families (values and templates, proposal linearity, authority and
// cost) are separate aggregates over the same graph. They do not join here
// — "structural" is the name of this family, not a stage in a pipeline.
// Values and Linearity are the other two that Validate runs; Authority and
// Cost take a host Env, so a caller that has one runs them itself.
func Structural(g *ir.Graph) []Diagnostic {
	if g == nil {
		return nil
	}
	var out []Diagnostic
	out = append(out, uniqueReferences(g)...)
	out = append(out, needsCycles(g)...)
	out = append(out, callDepth(g)...)
	out = append(out, unreachable(g)...)
	out = append(out, outputSpecs(g)...)
	return out
}

// Intrinsic runs the document-intrinsic families over a resolved graph and the
// document it came from, in catalog order: Structural, Values, Linearity. It is
// the shared middle of Validate, the subworkflow registry, and the CLI
// analysis, so adding an intrinsic family means changing one place rather than
// three. Host-relative families (Authority, Cost, Calls) take an Env the caller
// resolves from the live host and stay separate.
func Intrinsic(doc *spec.Document, g *ir.Graph) []Diagnostic {
	out := Structural(g)
	out = append(out, Values(doc, g)...)
	out = append(out, Linearity(g)...)
	return out
}

// Validate is the end-to-end seam: workflow document bytes in, the
// resolved graph and its diagnostics out. A document that strict decoding
// or resolution rejects never becomes a graph, so the failure is reported
// as one WF001 diagnostic through Diagnose; otherwise Structural runs
// first, Values runs after it, and Linearity runs last — all three over
// the same graph and the document it came from.
//
// Return contract: a document that parses and resolves yields a non-nil
// graph and diagnostics carrying only WF002–WF029 findings (possibly
// none); a document that never became a graph yields a nil graph and
// exactly one WF001. The graph is therefore nil exactly when the
// diagnostics contain a WF001. Findings stay grouped by rule in catalog
// order — WF005 before WF010 is numeric, not lexical — and in document
// order within a rule, so a caller can diff and display them without
// sorting. Callers that want to run a validated workflow should require
// zero diagnostics, which is what the spec goldens assert.
func Validate(data []byte) (*ir.Graph, []Diagnostic) {
	doc, err := spec.Parse(data)
	if err != nil {
		return nil, Diagnose(err)
	}
	g, err := ir.Resolve(doc)
	if err != nil {
		return nil, Diagnose(err)
	}
	return g, Intrinsic(doc, g)
}
