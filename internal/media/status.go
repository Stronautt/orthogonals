package media

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Status is the provisioning progress provision.ps1 writes to
// C:\orthogonals\provision-status.json after every stage.
type Status struct {
	Stage string `json:"stage"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Done reports whether every provisioning stage finished.
func (s Status) Done() bool { return s.OK && s.Stage == "done" }

// errNoStatus means the guest agent responded but the status file does not
// exist yet — provisioning has not started writing.
var errNoStatus = errors.New("guest has no provision status yet")

// ProvisionStatus reads the guest's provision-status.json through the qemu
// guest agent. The agent only responds once the virtio guest tools are in
// (provisioning stage 1) — before that virsh itself errors, so callers poll
// on a timeout without depending on the agent (plan Task 11).
func ProvisionStatus(vm string) (Status, error) {
	out, _, code, err := GuestExec(vm, "cmd.exe", "/c", "type", `C:\orthogonals\provision-status.json`)
	if err != nil {
		return Status{}, err
	}
	if code != 0 {
		return Status{}, errNoStatus
	}
	var st Status
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return Status{}, fmt.Errorf("parse guest provision status %q: %w", out, err)
	}
	return st, nil
}

// guestExecTries * guestExecInterval bounds how long GuestExec polls for the
// guest command to exit; `type` and nvidia-smi both finish near-instantly.
// Vars so the timeout path is testable without a 10 s sleep.
var (
	guestExecTries    = 50
	guestExecInterval = 200 * time.Millisecond
)

// GuestExec runs a command in the guest through the qemu guest agent
// (guest-exec + guest-exec-status via virsh) and returns its stdout, stderr,
// and exit code. verify (Task 11) reuses it for the in-guest nvidia-smi check.
func GuestExec(vm, path string, args ...string) (out, errOut []byte, exit int, err error) {
	var started struct {
		Return struct {
			Pid int `json:"pid"`
		} `json:"return"`
	}
	// a nil variadic marshals to JSON null and the agent rejects the command
	// ("Invalid parameter type for 'arg', expected: array")
	if args == nil {
		args = []string{}
	}
	err = agentCommand(vm, map[string]any{
		"execute":   "guest-exec",
		"arguments": map[string]any{"path": path, "arg": args, "capture-output": true},
	}, &started)
	if err != nil {
		return nil, nil, 0, err
	}

	var res struct {
		Return struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		} `json:"return"`
	}
	for range guestExecTries {
		err := agentCommand(vm, map[string]any{
			"execute":   "guest-exec-status",
			"arguments": map[string]any{"pid": started.Return.Pid},
		}, &res)
		if err != nil {
			return nil, nil, 0, err
		}
		if res.Return.Exited {
			out, err := base64.StdEncoding.DecodeString(res.Return.OutData)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("decode guest-exec output: %w", err)
			}
			errOut, err := base64.StdEncoding.DecodeString(res.Return.ErrData)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("decode guest-exec stderr: %w", err)
			}
			return out, errOut, res.Return.ExitCode, nil
		}
		time.Sleep(guestExecInterval)
	}
	return nil, nil, 0, fmt.Errorf("guest command %s did not exit within %v", path, time.Duration(guestExecTries)*guestExecInterval)
}

// AgentPing sends one guest-ping — the earliest signal that the virtio guest
// tools are up; callers own retry pacing.
func AgentPing(vm string) error {
	var resp map[string]any
	return agentCommand(vm, map[string]any{"execute": "guest-ping"}, &resp)
}

// agentCommand sends one qemu-guest-agent request via virsh and decodes the
// JSON reply into resp.
func agentCommand(vm string, req any, resp any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out, err := exec.Command("virsh", "qemu-agent-command", vm, string(b)).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return fmt.Errorf("virsh qemu-agent-command %s: %w\n%s", vm, err, bytes.TrimSpace(ee.Stderr))
		}
		return fmt.Errorf("virsh qemu-agent-command %s: %w", vm, err)
	}
	if err := json.Unmarshal(out, resp); err != nil {
		return fmt.Errorf("parse agent reply %q: %w", out, err)
	}
	return nil
}
