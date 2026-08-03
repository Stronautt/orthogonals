package steps

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// currentUserName is the one account a test can rely on existing, and whose
// uid it can chown to without privileges.
func currentUserName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

// noSessionBus makes the trust flag unavailable the way it is for a user who
// has never logged in, without depending on the developer's own session.
func noSessionBus(t *testing.T) {
	t.Helper()
	old := markTrusted
	markTrusted = func(out io.Writer, _, _ string, _, _ int) {
		_, _ = io.WriteString(out, DesktopTrustNote+"\n")
	}
	t.Cleanup(func() { markTrusted = old })
}

// The step must succeed when only the trust flag is unavailable: a defined
// domain and a created disk already exist by the time it runs.
func TestDesktopLinkSucceedsWithoutASession(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	owner := currentUserName(t)
	entry := "/usr/share/applications/win11.orthogonals.desktop"
	link := "/home/" + owner + "/Desktop/win11.orthogonals.desktop"

	var out strings.Builder
	err := opDesktopLink(nil, root, &out, map[string]string{
		"user": owner, "entry": entry, "link": link,
	})
	if err != nil {
		t.Fatalf("op failed though only the trust flag was unavailable: %v\n%s", err, out.String())
	}

	target, err := os.Readlink(filepath.Join(root, link))
	if err != nil {
		t.Fatalf("shortcut was not created: %v\n%s", err, out.String())
	}
	// Unprefixed on purpose: the shortcut resolves on the host, not inside the
	// test's --root tree.
	if target != entry {
		t.Errorf("shortcut points at %q, want %q", target, entry)
	}
	if !strings.Contains(out.String(), DesktopTrustNote) {
		t.Errorf("the skipped trust flag was not reported:\n%s", out.String())
	}
}

// Covers the re-run the engine performs when the journaled shortcut has gone
// missing (Step.CreatesPath).
func TestDesktopLinkIsIdempotent(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	owner := currentUserName(t)
	args := map[string]string{
		"user":  owner,
		"entry": "/usr/share/applications/win11.orthogonals.desktop",
		"link":  "/home/" + owner + "/Desktop/win11.orthogonals.desktop",
	}
	for i := range 2 {
		var out strings.Builder
		if err := opDesktopLink(nil, root, &out, args); err != nil {
			t.Fatalf("run %d failed: %v\n%s", i+1, err, out.String())
		}
	}
	if _, err := os.Readlink(filepath.Join(root, args["link"])); err != nil {
		t.Fatalf("shortcut missing after a second run: %v", err)
	}
}

// The tolerance is scoped to the trust flag: a shortcut that cannot be created
// must fail the op, not pass silently.
func TestDesktopLinkFailsLoudlyOnRealBreakage(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	owner := currentUserName(t)
	link := "/home/" + owner + "/Desktop/win11.orthogonals.desktop"
	// A regular file where the Desktop directory belongs, so MkdirAll fails.
	blocked := filepath.Join(root, filepath.Dir(link))
	if err := os.MkdirAll(filepath.Dir(blocked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := opDesktopLink(nil, root, &out, map[string]string{
		"user": owner, "entry": "/usr/share/applications/win11.orthogonals.desktop", "link": link,
	})
	if err == nil {
		t.Fatalf("op reported success though the shortcut could not be created:\n%s", out.String())
	}
}

// The user owns the shortcut directory and can replace it with a symlink.
// os.Chown follows one, handing them a root-owned directory — the op must
// refuse before it writes anything.
func TestDesktopLinkRefusesASymlinkedDesktop(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	owner := currentUserName(t)
	link := "/home/" + owner + "/Desktop/win11.orthogonals.desktop"

	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	desktop := filepath.Join(root, filepath.Dir(link))
	if err := os.MkdirAll(filepath.Dir(desktop), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, desktop); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := opDesktopLink(nil, root, &out, map[string]string{
		"user": owner, "entry": "/usr/share/applications/win11.orthogonals.desktop", "link": link,
	})
	if err == nil {
		t.Fatalf("op followed a symlinked shortcut directory:\n%s", out.String())
	}
	ents, rerr := os.ReadDir(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("op wrote into the symlink target before refusing: %v", ents)
	}
}

// opDesktopLink returns this error verbatim when root is "".
func TestLookupUserNamesTheUser(t *testing.T) {
	_, _, _, err := LookupUser("no-such-user-for-orthogonals")
	if err == nil {
		t.Fatal("LookupUser accepted a nonexistent account")
	}
	if !strings.Contains(err.Error(), "no-such-user-for-orthogonals") {
		t.Errorf("error does not name the user: %v", err)
	}
}

// Synthetic --root trees may name accounts the machine does not have: the
// shortcut is still created and the skipped ownership reported, not swallowed.
func TestDesktopLinkToleratesAnUnknownUserUnderRoot(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	link := "/home/nobody/Desktop/win11.orthogonals.desktop"
	var out strings.Builder
	err := opDesktopLink(nil, root, &out, map[string]string{
		"user":  "no-such-user-for-orthogonals",
		"entry": "/usr/share/applications/win11.orthogonals.desktop",
		"link":  link,
	})
	if err != nil {
		t.Fatalf("op refused a synthetic user under --root: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(root, link)); err != nil {
		t.Fatalf("shortcut was not created: %v", err)
	}
	if !strings.Contains(out.String(), "ownership not set") {
		t.Errorf("skipped ownership was not reported:\n%s", out.String())
	}
}

// The op contract undo replays from a fresh process.
func TestDesktopLinkNeedsItsArguments(t *testing.T) {
	for _, missing := range []string{"user", "entry", "link"} {
		args := map[string]string{
			"user":  "someone",
			"entry": "/usr/share/applications/win11.orthogonals.desktop",
			"link":  "/home/someone/Desktop/win11.orthogonals.desktop",
		}
		delete(args, missing)
		if err := opDesktopLink(nil, t.TempDir(), io.Discard, args); err == nil {
			t.Errorf("op accepted args with %q missing", missing)
		}
	}
}

// The real (unswapped) helper: with no session bus it must report and return,
// never fail.
func TestMarkTrustedReportsAMissingBus(t *testing.T) {
	var out strings.Builder
	// A uid that cannot have a runtime directory.
	markTrusted(&out, "/nonexistent/link.desktop", "/home/nobody", 999999, 999999)
	if !strings.Contains(out.String(), DesktopTrustNote) {
		t.Errorf("missing session bus was not reported:\n%s", out.String())
	}
}

func TestDesktopLinkReplacesAStaleShortcut(t *testing.T) {
	noSessionBus(t)
	root := t.TempDir()
	owner := currentUserName(t)
	link := "/home/" + owner + "/Desktop/win11.orthogonals.desktop"
	full := filepath.Join(root, link)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/share/applications/stale.desktop", full); err != nil {
		t.Fatal(err)
	}

	entry := "/usr/share/applications/win11.orthogonals.desktop"
	if err := opDesktopLink(nil, root, io.Discard, map[string]string{
		"user": owner, "entry": entry, "link": link,
	}); err != nil {
		t.Fatalf("op failed replacing a stale shortcut: %v", err)
	}
	target, err := os.Readlink(full)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			t.Fatalf("shortcut is gone: %v", err)
		}
		t.Fatal(err)
	}
	if target != entry {
		t.Errorf("stale shortcut survived: points at %q, want %q", target, entry)
	}
}
