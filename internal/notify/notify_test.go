package notify

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// TestSendRealPath exercises the real Send body with a fake notify-send on PATH.
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
