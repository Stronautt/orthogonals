package notify

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// fakePasswd covers ids no test machine is guaranteed to have.
func fakePasswd(t *testing.T, users map[string]*user.User) {
	t.Helper()
	old := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		u, ok := users[name]
		if !ok {
			return nil, user.UnknownUserError(name)
		}
		return u, nil
	}
	t.Cleanup(func() { lookupUser = old })
}

// The privilege drop itself: the hook calls Send as root, so this decides
// whether the notification runs as the desktop user or as root. The switching
// branch is unreachable from the other Send test, which uses the current account
// and so always has uid == euid.
func TestCredential(t *testing.T) {
	fakePasswd(t, map[string]*user.User{
		"desktop": {Uid: "1000", Gid: "1000"},
		"self":    {Uid: "500", Gid: "500"},
		"bad-uid": {Uid: "not-a-number", Gid: "1000"},
		"bad-gid": {Uid: "1000", Gid: "not-a-number"},
		// Past the 31-bit cap the int and uint32 conversions both have to hold.
		"huge-uid": {Uid: "4294967295", Gid: "1000"},
	})
	const euid = 500
	const desktopBus = "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"

	tests := []struct {
		name     string
		user     string
		wantErr  bool
		wantCred bool
		wantUID  uint32
		wantBus  string
	}{
		{name: "another user is switched to", user: "desktop", wantCred: true, wantUID: 1000, wantBus: desktopBus},
		{name: "own uid is not switched to", user: "self", wantBus: "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/500/bus"},
		{name: "unknown user", user: "ghost", wantErr: true},
		{name: "unusable uid", user: "bad-uid", wantErr: true},
		{name: "unusable gid", user: "bad-gid", wantErr: true},
		{name: "uid past the 31-bit cap", user: "huge-uid", wantErr: true},
	}
	for _, tt := range tests {
		// No t.Parallel: lookupUser is a process-global seam.
		t.Run(tt.name, func(t *testing.T) {
			cred, bus, err := credential(tt.user, euid)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("credential(%q) accepted the account", tt.user)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bus != tt.wantBus {
				t.Errorf("bus = %q, want %q", bus, tt.wantBus)
			}
			if !tt.wantCred {
				if cred != nil {
					t.Errorf("credential set for the current uid: %+v", cred.Credential)
				}
				return
			}
			if cred == nil || cred.Credential == nil {
				t.Fatal("no credential for a different uid — the notification would be sent as root")
			}
			if got := cred.Credential.Uid; got != tt.wantUID {
				t.Errorf("uid = %d, want %d", got, tt.wantUID)
			}
		})
	}
}

// The only place the switch actually executes: it needs root and a second
// account, which no unit run has. TestCredential covers the decision, this
// covers the kernel honouring it.
func TestSendDropsToAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching credentials needs root — covered by the VM tier (make test-vm)")
	}
	const target = "orthtest"
	u, err := user.Lookup(target)
	if err != nil {
		t.Skipf("no %s account on this host: %v", target, err)
	}

	// Not t.TempDir(): 0700 root, and the child runs as somebody else.
	dir, err := os.MkdirTemp("", "orthogonals-privdrop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "uid")
	stub := filepath.Join(dir, "notify-send")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nid -u > "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	Send(Notification{User: target, Title: "Windows VM", Body: "test body"})

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("notify-send never ran: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != u.Uid {
		t.Errorf("notify-send ran as uid %s, want %s (%s) — the notification kept root's credentials",
			got, u.Uid, target)
	}
}

func TestSendRealPath(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	tests := []struct {
		name     string
		user     string
		urgent   bool
		wantArgs []string
	}{
		{"in-session normal auto-hides", "", false, []string{"-u normal", "-t 5000"}},
		{"session owner critical stays", u.Username, true, []string{"-u critical", "-t 0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "notify.log")
			if err := os.WriteFile(filepath.Join(dir, "notify-send"),
				[]byte("#!/bin/sh\necho \"$*\" >> \""+log+"\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			Send(Notification{User: tt.user, Title: "Windows VM", Icon: "video-display", Urgent: tt.urgent, Body: "test body"})

			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("notify-send was not invoked: %v", err)
			}
			got := string(b)
			for _, want := range append([]string{"test body"}, tt.wantArgs...) {
				if !strings.Contains(got, want) {
					t.Errorf("notify-send args = %q, want to contain %q", got, want)
				}
			}
		})
	}
}
