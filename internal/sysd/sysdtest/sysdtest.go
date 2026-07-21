// Package sysdtest provides the scriptable in-process sysd.Client fake.
package sysdtest

import (
	"slices"
	"strings"
)

// Fake implements sysd.Client.
type Fake struct {
	Calls  []string
	States map[string]string
	Err    error
}

func (f *Fake) log(verb, unit string) { f.Calls = append(f.Calls, verb+" "+unit) }

// Logged reports whether any call matches the "verb unit" line.
func (f *Fake) Logged(call string) bool { return slices.Contains(f.Calls, call) }

func (f *Fake) EnableUnit(unit string) error {
	f.log("enable", unit)
	if f.Err != nil {
		return f.Err
	}
	if f.States == nil {
		f.States = map[string]string{}
	}
	f.States[unit] = "enabled"
	return nil
}

func (f *Fake) DisableUnit(unit string) error {
	f.log("disable", unit)
	if f.Err != nil {
		return f.Err
	}
	if f.States == nil {
		f.States = map[string]string{}
	}
	f.States[unit] = "disabled"
	return nil
}

func (f *Fake) UnitFileState(unit string) string {
	f.log("state", unit)
	if s, ok := f.States[unit]; ok {
		return s
	}
	return "unknown"
}

func (f *Fake) Reload() error {
	f.log("reload", "")
	return f.Err
}

func (f *Fake) RestartUnit(unit string) error {
	f.log("restart", unit)
	return f.Err
}

func (f *Fake) TryRestartUnit(unit string) error {
	f.log("try-restart", unit)
	return f.Err
}

func (f *Fake) StopUnit(unit string) error {
	f.log("stop", unit)
	return f.Err
}

func (f *Fake) ResetFailedUnit(unit string) error {
	f.log("reset-failed", unit)
	return f.Err
}

func (f *Fake) StartTransientUnit(name string, argv []string) error {
	f.Calls = append(f.Calls, "start-transient "+name+" "+strings.Join(argv, " "))
	return f.Err
}

func (f *Fake) Close() error { return nil }
