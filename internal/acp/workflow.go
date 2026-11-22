package acp

import (
	"encoding/json"

	"github.com/goatherdai/yaah/internal/cwdpath"
)

// handleWorkflowList implements workflow/list (workflowCapabilities.list):
// discovery of runnable workflow specs for a client's picker. The callback
// supplies the specs; the optional cwd is canonicalized here so a malformed
// workspace path surfaces as invalid params, not an internal error.
func (s *Server) handleWorkflowList(
	msg Message,
	respOK func(id json.RawMessage, result any),
	respErr func(id json.RawMessage, code int, message string),
) {
	if s.ListWorkflows == nil {
		respErr(msg.ID, codeMethodNotFound, "workflow/list is not available in this server configuration")
		return
	}
	var p struct {
		Cwd string `json:"cwd,omitempty"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			respErr(msg.ID, codeInvalidParams, "invalid params: "+err.Error())
			return
		}
	}
	cwd, err := cwdpath.Canonical(p.Cwd)
	if err != nil {
		respErr(msg.ID, codeInvalidParams, err.Error())
		return
	}
	specs, err := s.ListWorkflows(cwd)
	if err != nil {
		respErr(msg.ID, codeInternal, "list workflows: "+err.Error())
		return
	}
	if specs == nil {
		// An empty page is an empty array: `workflows: null` is not
		// schema-valid (WorkflowListResult requires the array).
		specs = []WorkflowInfo{}
	}
	respOK(msg.ID, WorkflowListResult{Workflows: specs})
}
