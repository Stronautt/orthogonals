// Package virttest provides the scriptable in-process virt.Client fake.
package virttest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/virt"
)

// Fake is a scriptable in-process virt.Client; the zero value is an unreachable hypervisor.
type Fake struct {
	Calls []string

	State   string
	UUID    string
	Phys    uint64
	OnState func() (string, error)
	Agent   func(cmd string) (string, error)
	XML     string

	MaxMemKiB    uint64
	DisplayHost  string
	DisplayPort  string
	DisplayAfter int
	displayCalls int

	StartErr, DestroyErr, ShutdownErr, KeyErr       error
	DefineErr, UndefineErr, NetErr, PingErr, VolErr error
}

// ErrNoDomain is what Fake methods return for a domain that does not exist.
var ErrNoDomain = errors.New("virttest: no such domain")

func (f *Fake) log(verb, name string) { f.Calls = append(f.Calls, verb+" "+name) }

// Logged reports whether any call matches the "verb name" line.
func (f *Fake) Logged(call string) bool { return slices.Contains(f.Calls, call) }

func (f *Fake) DefineDomain(xml string) error {
	f.Calls = append(f.Calls, "define")
	if f.DefineErr != nil {
		return fmt.Errorf("define domain: %w", f.DefineErr)
	}
	f.XML = xml
	if f.State == "" {
		f.State = "shut off"
	}
	return nil
}

func (f *Fake) UndefineDomain(name string) error {
	f.log("undefine", name)
	if f.UndefineErr != nil {
		return fmt.Errorf("undefine domain %s: %w", name, f.UndefineErr)
	}
	f.State = ""
	return nil
}

func (f *Fake) NetworkAutostart(name string) error {
	f.log("net-autostart", name)
	return f.NetErr
}

func (f *Fake) EnsureNetworkActive(name string) error {
	f.log("net-active", name)
	return f.NetErr
}

func (f *Fake) StartDomain(name string) error {
	f.log("start", name)
	if f.StartErr != nil {
		return fmt.Errorf("start domain %s: %w", name, f.StartErr)
	}
	f.State = "running"
	return nil
}

func (f *Fake) DestroyDomain(name string) error {
	f.log("destroy", name)
	if f.DestroyErr != nil {
		return fmt.Errorf("destroy domain %s: %w", name, f.DestroyErr)
	}
	f.State = "shut off"
	return nil
}

func (f *Fake) ShutdownDomain(name string) error {
	f.log("shutdown", name)
	if f.ShutdownErr != nil {
		return fmt.Errorf("shutdown domain %s: %w", name, f.ShutdownErr)
	}
	f.State = "shut off"
	return nil
}

func (f *Fake) DomainState(name string) (string, error) {
	f.log("domstate", name)
	if f.OnState != nil {
		return f.OnState()
	}
	if f.State == "" {
		return "", ErrNoDomain
	}
	return f.State, nil
}

func (f *Fake) DomainUUID(name string) (string, error) {
	f.log("domuuid", name)
	if f.UUID == "" {
		return "", ErrNoDomain
	}
	return f.UUID, nil
}

func (f *Fake) DomainBlockPhysical(name, dev string) (uint64, error) {
	f.log("domblkinfo", name+" "+dev)
	if f.State == "" && f.Phys == 0 {
		return 0, ErrNoDomain
	}
	return f.Phys, nil
}

func (f *Fake) DomainMaxMemoryKiB(name string) (uint64, error) {
	f.log("dommaxmem", name)
	if f.State == "" && f.MaxMemKiB == 0 {
		return 0, ErrNoDomain
	}
	return f.MaxMemKiB, nil
}

func (f *Fake) DomainDisplay(name string) (host, port string, err error) {
	f.displayCalls++
	f.log("domdisplay", name)
	if f.DisplayPort == "" || f.displayCalls <= f.DisplayAfter {
		return "", "", virt.ErrNoDisplay
	}
	return f.DisplayHost, f.DisplayPort, nil
}

func (f *Fake) SendKeyEnter(name string) error {
	f.log("send-key", name)
	return f.KeyErr
}

func (f *Fake) AgentCommand(name, cmdJSON string) (string, error) {
	f.log("agent", name+" "+cmdJSON)
	if f.Agent == nil {
		return "", fmt.Errorf("agent command to %s: %w", name, errors.New("guest agent is not connected"))
	}
	return f.Agent(cmdJSON)
}

func (f *Fake) CreateVolumeQCow2(path string, sizeGiB int) error {
	f.log("vol-create", fmt.Sprintf("%s %dG", path, sizeGiB))
	return f.VolErr
}

func (f *Fake) Ping() error {
	f.log("ping", "")
	return f.PingErr
}

func (f *Fake) Close() error { return nil }

// Responder scripts the standard happy-path guest agent.
func Responder(stdout, stderr string, exit int) func(string) (string, error) {
	ob := base64.StdEncoding.EncodeToString([]byte(stdout))
	eb := base64.StdEncoding.EncodeToString([]byte(stderr))
	return func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "guest-exec-status"):
			return fmt.Sprintf(`{"return":{"exited":true,"exitcode":%d,"out-data":"%s","err-data":"%s"}}`, exit, ob, eb), nil
		case strings.Contains(cmd, "guest-exec"):
			return `{"return":{"pid":7}}`, nil
		default:
			return `{"return":{}}`, nil
		}
	}
}
