package steps

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/virt"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// TestTrustCmd: the op runs as root, so gio without a credential writes into a
// user's home as root. markTrusted's other test only covers the no-bus return.
func TestTrustCmd(t *testing.T) {
	const link = "/home/alice/Desktop/win11.orthogonals.desktop"
	cmd := trustCmd(link, "/run/user/1000/bus", "/home/alice", 1000, 1000)

	if got := strings.Join(cmd.Args, " "); got != "gio set "+link+" metadata::trusted true" {
		t.Errorf("argv = %q", got)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("no credential — gio would run as root")
	}
	if c := cmd.SysProcAttr.Credential; c.Uid != 1000 || c.Gid != 1000 {
		t.Errorf("credential = %d:%d, want 1000:1000", c.Uid, c.Gid)
	}
	if want := "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"; !slices.Contains(cmd.Env, want) {
		t.Errorf("env missing %q", want)
	}
}

// The credential alone is not enough: sudo hands the op root's HOME, and gio
// then writes its gvfs metadata tree under /root — as a user who cannot.
func TestTrustCmdRepointsTheEnvironmentAtTheUser(t *testing.T) {
	inherited := []string{
		"HOME=/root",
		"XDG_RUNTIME_DIR=/run/user/0",
		"XDG_DATA_HOME=/root/.local/share",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/0/bus",
		"PATH=/usr/bin",
	}
	env := desktopEnv(inherited, "/home/alice", "/run/user/1000/bus", 1000)

	for _, gone := range inherited[:4] {
		if slices.Contains(env, gone) {
			t.Errorf("env kept root's %q — getenv takes the first match, so appending loses", gone)
		}
	}
	for _, want := range []string{
		"HOME=/home/alice",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"PATH=/usr/bin",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
}

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
