package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/virt"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

func defineStep() Step {
	return Step{
		ID: "vm-define-win11", Kind: KindOp,
		Op:       OpDefineDomain,
		Args:     map[string]string{"name": "win11", "xml": "/etc/orthogonals/vms/win11.xml"},
		Input:    []byte("<domain>v1</domain>"),
		UndoOp:   OpUndefineDomain,
		UndoArgs: map[string]string{"name": "win11"},
	}
}

func TestOpApplyJournalsAndUndoes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain>v1</domain>", 0o600)
	f := &virttest.Fake{}
	e, out, _ := eng(root, true)
	e.Virt = func() virt.Client { return f }

	if err := e.Apply([]Step{defineStep()}); err != nil {
		t.Fatal(err)
	}
	if f.XML != "<domain>v1</domain>" {
		t.Fatalf("define never reached the client, XML = %q", f.XML)
	}
	rec := mustLoad(t, root).find("vm-define-win11")
	if rec == nil || rec.Kind != KindOp || rec.Op != OpDefineDomain ||
		rec.UndoOp != OpUndefineDomain || rec.InputSHA256 == "" {
		t.Fatalf("journaled record = %+v", rec)
	}

	if err := e.Apply([]Step{defineStep()}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Calls); got != 1 {
		t.Fatalf("re-apply must be a no-op, calls = %v", f.Calls)
	}
	if !strings.Contains(out.String(), "already applied") {
		t.Fatalf("no-op re-apply should say already applied:\n%s", out.String())
	}

	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain>v2</domain>", 0o600)
	drifted := defineStep()
	drifted.Input = []byte("<domain>v2</domain>")
	if err := e.Apply([]Step{drifted}); err != nil {
		t.Fatal(err)
	}
	if f.XML != "<domain>v2</domain>" {
		t.Fatalf("input drift must re-define, XML = %q", f.XML)
	}

	renamed := drifted
	renamed.Args = map[string]string{"name": "other", "xml": "/etc/orthogonals/vms/win11.xml"}
	err := e.Apply([]Step{renamed})
	if err == nil || !strings.Contains(err.Error(), "undo first") {
		t.Fatalf("args drift must refuse, got %v", err)
	}

	found, err := e.UndoID("vm-define-win11", false)
	if err != nil || !found {
		t.Fatalf("UndoID = (%v, %v)", found, err)
	}
	if !f.Logged("undefine win11") {
		t.Fatalf("undo must undefine, calls = %v", f.Calls)
	}
	if mustLoad(t, root).Has("vm-define-win11") {
		t.Fatal("undone op must leave the manifest")
	}
}

func TestOpDryRunNeverDials(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain>v1</domain>", 0o600)
	e, out, _ := eng(root, false)
	e.Virt = func() virt.Client { t.Fatal("dry run dialed libvirt"); return nil }
	e.Sysd = func() sysd.Client { t.Fatal("dry run dialed systemd"); return nil }
	if err := e.Apply([]Step{defineStep()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would: define-domain name=win11 xml=/etc/orthogonals/vms/win11.xml") {
		t.Fatalf("dry-run output missing the op line:\n%s", out.String())
	}
}

func TestOpSkipsUnderUninjectedRoot(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain>v1</domain>", 0o600)
	var out, errBuf strings.Builder
	e := &Engine{Root: root, Yes: true, Out: &out, Err: &errBuf} // no injected clients
	if err := e.Apply([]Step{defineStep()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skipped under --root") {
		t.Fatalf("op must skip under an un-injected root:\n%s", out.String())
	}
	if !mustLoad(t, root).Has("vm-define-win11") {
		t.Fatal("skipped op must still be journaled")
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "undo skipped under --root") {
		t.Fatalf("op undo must skip under an un-injected root:\n%s", out.String())
	}
	if len(mustLoad(t, root).Records) != 0 {
		t.Fatal("skipped undo must still clear the record")
	}
}

func TestRemoveFileOpRespectsRoot(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/local/bin/staged-file", "elf\n", 0o755)
	var out, errBuf strings.Builder
	e := &Engine{Root: root, Yes: true, Out: &out, Err: &errBuf}
	step := Step{
		ID: "remove-staged-file", Kind: KindRunCmd,
		Cmd:      []string{"true"},
		UndoOp:   OpRemoveFile,
		UndoArgs: map[string]string{"path": "/usr/local/bin/staged-file"},
	}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/local/bin/staged-file")); err == nil {
		t.Fatal("undo must remove the rooted file")
	}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatalf("remove-file of an absent path must succeed: %v", err)
	}
}

func TestUndoRunsOpsAfterFileRestores(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	var out, errBuf strings.Builder
	e := &Engine{Root: root, Yes: true, Out: &out, Err: &errBuf}
	list := []Step{
		{ID: "conf", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o644},
		{ID: "marker", Kind: KindRunCmd, Cmd: []string{"true"},
			UndoOp: OpRemoveFile, UndoArgs: map[string]string{"path": "/etc/foo.marker"}},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	restore, op := strings.Index(s, "restored /etc/foo.conf"), strings.Index(s, "removed /etc/foo.marker")
	if restore == -1 || op == -1 || op < restore {
		t.Fatalf("op undo must run after file restores:\n%s", s)
	}
}
