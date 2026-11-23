package tools

// The evidence-window regression (yaah-1615).
//
// A self-guarding tool takes the per-path lock inside its own Execute, so the
// recording wrapper cannot hold it across the call. Before the onCommit hook the
// wrapper took its after-image AFTER the tool had unlocked, which meant a
// concurrent dispatch's write could land in that gap and be recorded as this
// dispatch's change: the bytes on disk were right, the attribution was wrong.
// These tests pin the fixed behaviour by reproducing the gap deterministically.

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// postMutationProbe is a workspace double that turns a recorder's post-write
// read into the exact window the regression needs. It fires an intruding
// dispatch's write when — and only when — a probed path is read after the tool
// mutated it AND the per-path lock is no longer held: while the lock is held the
// image is provably this dispatch's, and with the fix that is where every image
// is captured.
type postMutationProbe struct {
	Workspace

	// targets are the paths this probe watches (a transfer watches both ends).
	targets   []string
	onIntrude func()

	mu        sync.Mutex
	mutations int
	injected  bool
}

func sameProbedPath(a, b string) bool { return resolvedPath(a) == resolvedPath(b) }

func (p *postMutationProbe) probed(path string) bool {
	for _, target := range p.targets {
		if sameProbedPath(path, target) {
			return true
		}
	}
	return false
}

func (p *postMutationProbe) note(path string) {
	if !p.probed(path) {
		return
	}
	p.mu.Lock()
	p.mutations++
	p.mu.Unlock()
}

// shouldIntrude reports whether this read IS the regression window: a read of a
// mutated target with the per-path lock released.
func (p *postMutationProbe) shouldIntrude(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mutations == 0 || p.injected {
		return false
	}
	if pathLockHeld(path) {
		return false
	}
	p.injected = true
	return true
}

func (p *postMutationProbe) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	err := p.Workspace.WriteFile(ctx, path, data, perm)
	if err == nil {
		p.note(path)
	}
	return err
}

func (p *postMutationProbe) Copy(ctx context.Context, src, dst string) error {
	err := p.Workspace.Copy(ctx, src, dst)
	if err == nil {
		p.note(dst)
	}
	return err
}

func (p *postMutationProbe) Move(ctx context.Context, src, dst string) error {
	err := p.Workspace.Move(ctx, src, dst)
	if err == nil {
		p.note(src)
		p.note(dst)
	}
	return err
}

func (p *postMutationProbe) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if p.probed(path) && p.shouldIntrude(path) {
		p.onIntrude()
	}
	return p.Workspace.ReadFile(ctx, path)
}

func (p *postMutationProbe) fired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.injected
}

// intrudingDispatch is the second dispatch: a recording write of different bytes
// to the probed path, so a misattributed after-image is distinguishable by its
// digest. It runs on its own workspace and tracker.
type intrudingDispatch struct {
	rt   *RecordingTool
	args string

	mu      sync.Mutex
	err     error
	blocked bool
	started bool
}

func newIntrudingDispatch(t *testing.T, pv *PathValidator, relPath, content string) *intrudingDispatch {
	t.Helper()
	rt := NewRecordingTool(&WriteTool{}, &ConflictTracker{})
	rt.SetPathValidator(pv)
	rt.SetWorkspace(NewLocalWorkspace(pv))
	args, err := json.Marshal(map[string]any{
		"filePath":  relPath,
		"content":   content,
		"overwrite": true,
	})
	if err != nil {
		t.Fatalf("marshal intruder args: %v", err)
	}
	return &intrudingDispatch{rt: rt, args: string(args)}
}

// fire runs the second dispatch and waits for it. It runs at most once. The wait
// is bounded: a dispatch that cannot complete means the recorder is holding the
// per-path lock across its capture, which is a different defect and must fail
// loudly instead of hanging the suite.
func (d *intrudingDispatch) fire() {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := d.rt.Execute(WithConflictDispatch(context.Background(), "worker b", ConflictLaneShared), d.args)
		done <- err
	}()
	select {
	case err := <-done:
		d.mu.Lock()
		d.err = err
		d.mu.Unlock()
	case <-time.After(10 * time.Second):
		d.mu.Lock()
		d.blocked = true
		d.mu.Unlock()
	}
}

func (d *intrudingDispatch) result() (blocked bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked, d.err
}

// TestRecordingTool_PostWriteImageIsThisDispatchsWrite pins the fix: dispatch A
// writes the path, then dispatch B writes it inside the window A's own after-read
// would have used. A's evidence must describe A's bytes.
//
// The two cases cover both publish points — the withFileWrite onCommit hook
// (write) and a tool that publishes from inside its own lock (copy) — because
// they are separate code paths.
func TestRecordingTool_PostWriteImageIsThisDispatchsWrite(t *testing.T) {
	const alpha, bravo = "alpha-content\n", "bravo-content\n"

	cases := []struct {
		name    string
		tool    Tool
		prepare func(t *testing.T, dir string)
		args    string
	}{
		{
			// The mutation goes through withFileWrite, so the guard's onCommit
			// hook publishes the image under the lock.
			name: "write via withFileWrite hook",
			tool: &WriteTool{},
			args: `{"filePath":"shared.txt","content":"alpha-content\n"}`,
		},
		{
			// The mutation is a substrate copy, so the tool publishes the
			// destination itself while it holds the lock.
			name: "copy publishes its destination",
			tool: &CopyTool{},
			prepare: func(t *testing.T, dir string) {
				writeFileAt(t, filepath.Join(dir, "src.txt"), alpha)
			},
			args: `{"path":"src.txt","to":"shared.txt"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pv := NewPathValidator(dir, false, nil)
			pv.WorkDir = pv.WorkspaceRoot
			if tc.prepare != nil {
				tc.prepare(t, pv.WorkspaceRoot)
			}
			target := filepath.Join(pv.WorkspaceRoot, "shared.txt")

			intruder := newIntrudingDispatch(t, pv, "shared.txt", bravo)
			probe := &postMutationProbe{
				Workspace: NewLocalWorkspace(pv),
				targets:   []string{target},
				onIntrude: intruder.fire,
			}

			trackerA := &ConflictTracker{}
			rtA := NewRecordingTool(tc.tool, trackerA)
			rtA.SetPathValidator(pv)
			rtA.SetWorkspace(probe)
			if _, err := rtA.Execute(WithConflictDispatch(context.Background(), "worker a", ConflictLaneShared), tc.args); err != nil {
				t.Fatalf("dispatch A: %v", err)
			}
			// The recorder used the published image, so nothing read the target
			// outside the lock and the intruder never fired: run it now.
			if !probe.fired() {
				intruder.fire()
			}
			if blocked, err := intruder.result(); blocked || err != nil {
				t.Fatalf("dispatch B blocked = %v, err = %v; the evidence capture must not hold the per-path lock", blocked, err)
			}
			// Precondition: B really did replace A's bytes, so the digest
			// assertion below cannot pass vacuously.
			assertFileContent(t, target, bravo)

			trackerA.mu.Lock()
			records := append([]FileRecord(nil), trackerA.records...)
			trackerA.mu.Unlock()
			if len(records) != 1 {
				t.Fatalf("A recorded %d change(s), want exactly its own write: %+v", len(records), records)
			}
			record := records[0]
			if record.BeforeExists {
				t.Errorf("A's record = %+v, want the path created by A", record)
			}
			if want := proposalContentDigest([]byte(alpha)); record.AfterDigest != want {
				t.Errorf("A's after-digest = %s, want its own write %s: the recorder captured the path after the tool released its lock and picked up the concurrent dispatch's write (yaah-1615)",
					record.AfterDigest, want)
			}
			if !record.Known {
				t.Errorf("A's record = %+v, want verified content", record)
			}
		})
	}
}

// TestRecordingTool_MovePublishesBothEnds pins the transfer publish point for
// move: both ends are published from inside the tool's lock, so neither record
// can be attributed to a concurrent writer and the source record still reports
// the path gone.
func TestRecordingTool_MovePublishesBothEnds(t *testing.T) {
	const payload, bravo = "moved-payload\n", "bravo-content\n"
	dir := t.TempDir()
	pv := NewPathValidator(dir, false, nil)
	pv.WorkDir = pv.WorkspaceRoot
	src := filepath.Join(pv.WorkspaceRoot, "src.txt")
	dst := filepath.Join(pv.WorkspaceRoot, "dst.txt")
	writeFileAt(t, src, payload)

	intruder := newIntrudingDispatch(t, pv, "dst.txt", bravo)
	probe := &postMutationProbe{
		Workspace: NewLocalWorkspace(pv),
		targets:   []string{src, dst},
		onIntrude: intruder.fire,
	}
	tracker := &ConflictTracker{}
	rt := NewRecordingTool(&MoveTool{}, tracker)
	rt.SetPathValidator(pv)
	rt.SetWorkspace(probe)

	if _, err := rt.Execute(WithConflictDispatch(context.Background(), "worker a", ConflictLaneShared),
		`{"path":"src.txt","to":"dst.txt"}`); err != nil {
		t.Fatalf("move: %v", err)
	}

	// The records are complete when Execute returns; read them before the
	// intruder (which overwrites the destination) runs.
	tracker.mu.Lock()
	records := append([]FileRecord(nil), tracker.records...)
	tracker.mu.Unlock()
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after the move (err = %v)", err)
	}
	assertFileContent(t, dst, payload)
	fired := probe.fired()
	if !fired {
		intruder.fire()
	}

	if fired {
		t.Fatalf("the recorder read a moved path after the tool released its lock (records = %+v): move must publish both ends from inside the lock", records)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want both ends of the move: %+v", len(records), records)
	}
	wantPayload := proposalContentDigest([]byte(payload))
	for _, record := range records {
		switch record.FilePath {
		case "dst.txt":
			if record.AfterDigest != wantPayload || !record.Known {
				t.Errorf("dst record = %+v, want the moved payload", record)
			}
		case "src.txt":
			if record.AfterExists || !record.Known {
				t.Errorf("src record = %+v, want the source gone", record)
			}
		default:
			t.Errorf("unexpected record path %q", record.FilePath)
		}
	}
	if blocked, err := intruder.result(); blocked || err != nil {
		t.Fatalf("intruding move-target write: blocked = %v, err = %v", blocked, err)
	}
}
