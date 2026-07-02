package media

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A no-arg guest command (verify's nvidia-smi) must still send arg as an empty
// JSON array: a nil variadic marshals to null and the agent refuses the
// command with "Invalid parameter type for 'arg', expected: array".
func TestGuestExecSendsEmptyArgArray(t *testing.T) {
	log := fakeBin(t, fakePath(t), "virsh", `case "$*" in
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":0,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	if _, _, _, err := GuestExec("win11", `C:\Windows\System32\nvidia-smi.exe`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), `"arg":[]`) {
		t.Errorf(`guest-exec must send "arg":[] for a no-arg command:\n%s`, b)
	}
}

func TestGuestExecTimeout(t *testing.T) {
	oldTries, oldInterval := guestExecTries, guestExecInterval
	guestExecTries, guestExecInterval = 2, time.Millisecond
	t.Cleanup(func() { guestExecTries, guestExecInterval = oldTries, oldInterval })
	// guest-exec accepted, but the command never reports exited
	fakeBin(t, fakePath(t), "virsh", `case "$*" in
*guest-exec-status*) echo '{"return":{"exited":false}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	_, _, _, err := GuestExec("win11", "hang.exe")
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("want hung-command timeout, got %v", err)
	}
}
