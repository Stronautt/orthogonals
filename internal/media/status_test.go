package media

import (
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

func TestGuestExecSendsEmptyArgArray(t *testing.T) {
	f := agentFake("", 0)
	if _, _, _, err := GuestExec(f, "win11", `C:\Windows\System32\nvidia-smi.exe`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Calls, "\n"), `"arg":[]`) {
		t.Errorf(`guest-exec must send "arg":[] for a no-arg command:\n%v`, f.Calls)
	}
}

func TestGuestExecTimeout(t *testing.T) {
	oldTries, oldInterval := guestExecTries, guestExecInterval
	guestExecTries, guestExecInterval = 2, time.Millisecond
	t.Cleanup(func() { guestExecTries, guestExecInterval = oldTries, oldInterval })
	f := &virttest.Fake{State: "running", Agent: func(cmd string) (string, error) {
		if strings.Contains(cmd, "guest-exec-status") {
			return `{"return":{"exited":false}}`, nil
		}
		return `{"return":{"pid":7}}`, nil
	}}
	_, _, _, err := GuestExec(f, "win11", "hang.exe")
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("want hung-command timeout, got %v", err)
	}
}
