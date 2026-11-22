package yaah

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/goatherdai/yaah/internal/workflow/discover"
	"github.com/spf13/cobra"
)

// workflow_list.go is `yaah workflow list` (workflow default-location
// feature): discover the documents in the search directories without opening
// any of them.

func newWorkflowListCmd() *cobra.Command {
	var dirs []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List discovered workflow specs",
		Long: `List the workflow documents found in the search directories, in order:
--workflow-dir (repeatable), then ./workflows, then ~/.yaah/workflows.
Documents are deduplicated by base name — an earlier directory wins — and
printed name first, then path.

--json shape:
  [ { "name": "foo", "path": "/abs/foo.yaml", "dir": "/abs" } ]`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			specs, err := discover.List(cwd, dirs...)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeWorkflowListJSON(out, specs)
			}
			renderWorkflowList(out, specs)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "workflow-dir", nil,
		"prepend a directory to the search path (repeatable; the defaults are ./workflows then ~/.yaah/workflows)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON array of discovered specs instead of the text table")
	return cmd
}

func renderWorkflowList(out io.Writer, specs []discover.Spec) {
	if len(specs) == 0 {
		fmt.Fprintln(out, "no workflow specs found")
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH")
	for _, s := range specs {
		fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Path)
	}
	w.Flush()
}

func writeWorkflowListJSON(out io.Writer, specs []discover.Spec) error {
	if specs == nil {
		specs = []discover.Spec{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(specs)
}
