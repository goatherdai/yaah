// cmdnames.go — the names of the tools cmd/yaah registers outside
// internal/tools' built-in inventory. cmd/yaah owns these tools (the
// workflow pair) because internal/workflow/host imports
// internal/tools and a tool living there would close an import cycle —
// so they never appear in tools.Policies(), the inventory that tracks
// every other built-in.
//
// The constants are the single spelling both sides read: the tools'
// Name() methods, their belt admissions, and the session driver's
// registry lookups all use them, and the TUI's icon drift test iterates
// CmdLevelToolNames() so a newly added cmd-level tool fails coverage
// until it gets a distinct icon and color.
package toolcontract

// Cmd-level tool names, keyed exactly as the model and transcript see
// them.
const (
	ToolWorkflowRun  = "workflow_run"
	ToolWorkflowList = "workflow_list"
)

// CmdLevelToolNames returns every cmd-level tool name. Consumers that
// need "all tools everywhere" take this beside tools.Policies().
func CmdLevelToolNames() []string {
	return []string{ToolWorkflowRun, ToolWorkflowList}
}
