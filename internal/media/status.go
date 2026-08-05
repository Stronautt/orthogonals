package media

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stronautt/orthogonals/internal/virt"
)

// Status is the provisioning progress provision.ps1 writes after every stage.
type Status struct {
	Stage string `json:"stage"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (s Status) Done() bool { return s.OK && s.Stage == "done" }

// errNoStatus means the agent responded but the status file does not exist yet.
var errNoStatus = errors.New("guest has no provision status yet")

func ProvisionStatus(c virt.Client, vm string) (Status, error) {
	out, _, code, err := GuestExec(c, vm, "cmd.exe", "/c", "type", `C:\orthogonals\provision-status.json`)
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

// guestExecTries * guestExecInterval bounds how long GuestExec polls.
var (
	guestExecTries    = 50
	guestExecInterval = 200 * time.Millisecond
)

func GuestExec(c virt.Client, vm, path string, args ...string) (out, errOut []byte, exit int, err error) {
	var started struct {
		Return struct {
			Pid int `json:"pid"`
		} `json:"return"`
	}
	if args == nil {
		args = []string{}
	}
	err = agentCommand(c, vm, map[string]any{
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
		err := agentCommand(c, vm, map[string]any{
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

// StartLGHost starts the guest's Looking Glass host service when it is stopped.
//
// The service stops itself for good when its host program fails to start, and
// it reports a clean stop. So the SCM runs no recovery action and nothing in
// the guest starts it again. A start is enough, and a restart would tear down
// a healthy capture: the service stays up for as long as it retries, and only
// the last failure stops it.
func StartLGHost(c virt.Client, vm string) error {
	out, errOut, code, err := GuestExec(c, vm, "powershell.exe", "-NoProfile", "-Command",
		"Start-Service -Name '"+LGHostServiceName+"'")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("start %q in the guest: exit %d — %s", LGHostServiceName, code,
			bytes.TrimSpace(append(out, errOut...)))
	}
	return nil
}

func AgentPing(c virt.Client, vm string) error {
	var resp map[string]any
	return agentCommand(c, vm, map[string]any{"execute": "guest-ping"}, &resp)
}

func agentCommand(c virt.Client, vm string, req any, resp any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out, err := c.AgentCommand(vm, string(b))
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), resp); err != nil {
		return fmt.Errorf("parse agent reply %q: %w", out, err)
	}
	return nil
}
