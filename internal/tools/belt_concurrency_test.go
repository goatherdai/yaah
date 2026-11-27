package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The concurrency contract these tests pin: a batch's tool calls execute
// concurrently on the agent loop (executeAndCollect fans out one goroutine
// per call), and the lazy toolbelt registers at runtime from inside a tool
// call. A belt or registry write racing another write — or racing a read —
// is a fatal (concurrent map writes / read during write), not a data race:
// 0.222.0 died exactly there, in Belt.Admit, when a smoke-test batch
// mounted two toolbelt families in one response.

// TestBelt_ConcurrentAdmitAndRead hammers every belt surface from several
// goroutines. Unsynchronized, the overlapping map writes are the runtime's
// concurrent-map-writes fatal; under -race any remaining access race fails
// deterministically.
func TestBelt_ConcurrentAdmitAndRead(t *testing.T) {
	belt := NewBelt([]string{"read"})

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				name := fmt.Sprintf("tool-%d", w*100+i%20)
				belt.Admit(name)
				belt.Know(name)
				belt.Refuse(fmt.Sprintf("ref-%d", i%8))
				belt.Allows(name)
				belt.Knows(name)
				belt.Refused()
				belt.Unknown()
				belt.Allowed()
			}
		}(w)
	}
	wg.Wait()

	if !belt.Allows("tool-0") {
		t.Error("an admitted name must be allowed after concurrent traffic")
	}
	if !belt.Knows("tool-0") {
		t.Error("an admitted name must be known after concurrent traffic")
	}
}

// TestRegistry_ConcurrentRegisterAndExecute drives the registry's two
// concurrent sides: writers admitting and registering the way a toolbelt
// mount does, and readers executing and listing the way the sibling calls
// of the same batch do.
func TestRegistry_ConcurrentRegisterAndExecute(t *testing.T) {
	belt := NewBelt([]string{"read"})
	reg := NewRegistryWithBelt(belt)
	if read := NewLeafTool("read"); read != nil {
		reg.Register(read)
	}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				reg.Admit("write")
				reg.Register(&WriteTool{})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				reg.Execute(context.Background(), "read", `{}`)
				reg.Get("write")
				reg.List()
				reg.Generation()
			}
		}()
	}
	wg.Wait()

	if reg.Get("write") == nil {
		t.Error("the concurrently registered tool must be registered")
	}
	if reg.Generation() <= 0 {
		t.Error("generation must have advanced")
	}
}

// TestToolbelt_ConcurrentMounts reproduces the 0.222.0 fatal's exact
// shape: one batch, two toolbelt mount calls, executed concurrently the
// way the agent loop executes a batch. Two mounts racing on one belt and
// one registry killed the process (fatal error: concurrent map writes in
// substrate.Belt.Admit); with the synchronized belt and registry both
// mounts succeed and every member of both families is callable.
func TestToolbelt_ConcurrentMounts(t *testing.T) {
	belt := NewBelt([]string{"toolbelt"})
	reg := NewRegistryWithBelt(belt)
	tb := &ToolbeltTool{Reg: reg}

	families := []string{"file_ops", "analysis"}
	var wg sync.WaitGroup
	for _, fam := range families {
		wg.Add(1)
		go func(fam string) {
			defer wg.Done()
			args := fmt.Sprintf(`{"action":"mount","family":%q}`, fam)
			if _, err := tb.Execute(context.Background(), args); err != nil {
				t.Errorf("concurrent mount of %s failed: %v", fam, err)
			}
		}(fam)
	}
	// Sibling calls of the same batch: lookups and executions while the
	// mounts register.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			reg.List()
			reg.Get("toolbelt")
		}
	}()
	wg.Wait()

	for _, fam := range toolFamilies {
		if fam.Name != "file_ops" && fam.Name != "analysis" {
			continue
		}
		for _, name := range fam.Tools {
			if reg.Get(name) == nil {
				t.Errorf("family %s member %s not registered after concurrent mounts", fam.Name, name)
			}
			if !belt.Allows(name) {
				t.Errorf("family %s member %s not admitted after concurrent mounts", fam.Name, name)
			}
		}
	}
}

// TestToolbelt_SequentialMountStillAtomic pins that the mount's own
// contract survived the synchronization: a refused-everything mount
// leaves the belt exactly as it found it, and a successful mount's
// members are all callable.
func TestToolbelt_SequentialMountStillAtomic(t *testing.T) {
	belt := NewBelt([]string{"toolbelt"})
	reg := NewRegistryWithBelt(belt)
	// A persona that withholds every member of the analysis family:
	// the mount must fail atomically and widen nothing.
	tb := &ToolbeltTool{
		Reg: reg,
		Admits: func(name string) (bool, string) {
			return false, "persona"
		},
	}
	before := belt.Allowed()
	if _, err := tb.Execute(context.Background(), `{"action":"mount","family":"analysis"}`); err == nil {
		t.Fatal("a mount refused by Admits must fail")
	}
	if got, want := strings.Join(belt.Allowed(), ","), strings.Join(before, ","); got != want {
		t.Errorf("a failed mount widened the belt: before %q, after %q", want, got)
	}
}
