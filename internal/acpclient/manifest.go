// Package acpclient drives external ACP agents as jobs.TaskRunner backends.
//
// It is a leaf package: it imports the ACP SDK, the jobs contract, and the
// shared message/usage types. It does not import internal/tools, internal/acp,
// or the agent loop, so it can be wired from either the delegate tool or the
// workflow host without a cycle.
package acpclient

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Manifest describes one external ACP agent that yaah can launch as a child
// process. Name is the stable routing key; Command is the executable spoken
// to over stdio.
type Manifest struct {
	Name    string
	Command string
	Args    []string
	Env     []string
	Cwd     string
}

// Validate checks the fields needed to spawn the agent.
func (m Manifest) Validate() error {
	if m.Command == "" {
		return errors.New("acpclient: manifest command is required")
	}
	if m.Cwd != "" && !filepath.IsAbs(m.Cwd) {
		return fmt.Errorf("acpclient: manifest cwd must be absolute, got %q", m.Cwd)
	}
	for _, entry := range m.Env {
		if entry == "" {
			return errors.New("acpclient: empty env entry")
		}
	}
	return nil
}
