// Package supervision holds the locked meta-agent prompt assets for the
// supervisor policy (docs/supervisor-policy-design.md §4.4, yaah-wego).
//
// Three packages in this tree are named `supervision`: this asset holder
// (internal/prompts/supervision), the renderer (internal/supervision), and
// the agent-side snapshot builder (internal/agent/supervision). The collision
// is accepted rather than renamed — the design doc fixes all three import
// paths — so a file that imports two of them must alias one deliberately.
//
// The assets here are byte-exact upstream copies. Nothing in this package
// renders, trims, or substitutes anything: the scope labels an asset's
// protocol refers to are substituted into the RENDERED SNAPSHOT
// (internal/supervision.Render), not into the system prompt.
package supervision

import _ "embed"

// MetaPromptV1Name is the config token naming this prompt version
// (supervisor.prompt: meta_prompt_v1).
const MetaPromptV1Name = "meta_prompt_v1"

// MetaPromptV1 is the Shepherd paper's locked v1 meta-agent prompt, verbatim
// (shepherd-agents/shepherd-experiments exp/live-intervention/prompts/meta_prompt_v1.txt,
// upstream blob 585c6923abd5a5ba749b2d683b1c119508142fb4). It must never be
// edited, trimmed, or re-flowed; scope ids are substituted in the RENDERED
// SNAPSHOT (internal/supervision.Render), not in this system prompt.
//
//go:embed meta_prompt_v1.md
var MetaPromptV1 string
