// Package sysd is the systemd seam over the manager's private D-Bus socket.
package sysd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

type Client interface {
	// EnableUnit and DisableUnit change unit-file symlinks and reload the manager.
	EnableUnit(unit string) error
	DisableUnit(unit string) error
	// UnitFileState reports enabled/disabled/static/…; errors degrade to "unknown".
	UnitFileState(unit string) string
	Reload() error
	RestartUnit(unit string) error
	TryRestartUnit(unit string) error
	// StopUnit and ResetFailedUnit treat an absent unit as success.
	StopUnit(unit string) error
	ResetFailedUnit(unit string) error
	StartTransientUnit(name string, argv []string) error
	// SetAllowedCPUs restricts a unit's cgroup cpuset at runtime (cgroup v2
	// AllowedCPUs); passing the full CPU set lifts a prior restriction.
	SetAllowedCPUs(unit string, cpus []int) error
	Close() error
}

// New returns a client for the local systemd manager, one connection per call.
func New() Client { return &client{} }

const callTimeout = time.Minute

type client struct{}

// do runs one manager call on its own connection, never cached across calls:
// go-systemd binds a connection's lifetime to the context it was dialled with,
// so one dialled here is closed the moment this call's context is cancelled. A
// later call reusing it either redials mid-flight or — slipping through just
// before the close lands — blocks until its own deadline for a JobRemoved
// signal the dead connection can no longer deliver.
func (c *client) do(op string, f func(ctx context.Context, conn *sddbus.Conn) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	conn, err := sddbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd (root required): %w", err)
	}
	defer conn.Close()
	if err := f(ctx, conn); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func (c *client) EnableUnit(unit string) error {
	return c.do("enable unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		if _, _, err := conn.EnableUnitFilesContext(ctx, []string{unit}, false, false); err != nil {
			return err
		}
		return conn.ReloadContext(ctx)
	})
}

func (c *client) DisableUnit(unit string) error {
	return c.do("disable unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		if _, err := conn.DisableUnitFilesContext(ctx, []string{unit}, false); err != nil {
			return err
		}
		return conn.ReloadContext(ctx)
	})
}

func (c *client) UnitFileState(unit string) string {
	state := ""
	err := c.do("unit state", func(ctx context.Context, conn *sddbus.Conn) error {
		p, err := conn.GetUnitPropertyContext(ctx, unit, "UnitFileState")
		if err != nil {
			return err
		}
		if s, ok := p.Value.Value().(string); ok {
			state = s
		}
		return nil
	})
	if err != nil || state == "" {
		return "unknown"
	}
	return state
}

func (c *client) Reload() error {
	return c.do("daemon-reload", func(ctx context.Context, conn *sddbus.Conn) error {
		return conn.ReloadContext(ctx)
	})
}

func (c *client) RestartUnit(unit string) error {
	return c.restart("restart unit "+unit, unit, (*sddbus.Conn).RestartUnitContext)
}

func (c *client) TryRestartUnit(unit string) error {
	return c.restart("try-restart unit "+unit, unit, (*sddbus.Conn).TryRestartUnitContext)
}

func (c *client) restart(op, unit string, call func(*sddbus.Conn, context.Context, string, string, chan<- string) (int, error)) error {
	return c.do(op, func(ctx context.Context, conn *sddbus.Conn) error {
		return waitJob(ctx, func(done chan string) (int, error) {
			return call(conn, ctx, unit, "replace", done)
		})
	})
}

func (c *client) StopUnit(unit string) error {
	return c.do("stop unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		err := waitJob(ctx, func(done chan string) (int, error) {
			return conn.StopUnitContext(ctx, unit, "replace", done)
		})
		if isNoSuchUnit(err) {
			return nil
		}
		return err
	})
}

func (c *client) ResetFailedUnit(unit string) error {
	return c.do("reset-failed unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		err := conn.ResetFailedUnitContext(ctx, unit)
		if isNoSuchUnit(err) {
			return nil
		}
		return err
	})
}

func (c *client) StartTransientUnit(name string, argv []string) error {
	return c.do("start transient unit "+name, func(ctx context.Context, conn *sddbus.Conn) error {
		props := []sddbus.Property{
			sddbus.PropExecStart(argv, false),
			sddbus.PropDescription("orthogonals: " + name),
		}
		return waitJob(ctx, func(done chan string) (int, error) {
			return conn.StartTransientUnitContext(ctx, name, "replace", props, done)
		})
	})
}

func (c *client) SetAllowedCPUs(unit string, cpus []int) error {
	return c.do("set AllowedCPUs "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		return conn.SetUnitPropertiesContext(ctx, unit, true, sddbus.Property{
			Name:  "AllowedCPUs",
			Value: godbus.MakeVariant(allowedCPUsMask(cpus)),
		})
	})
}

// allowedCPUsMask encodes cpus as systemd's AllowedCPUs value: a little-endian
// bitmask where bit N (byte N/8, bit N%8) is set when CPU N is allowed.
func allowedCPUsMask(cpus []int) []byte {
	high := -1
	for _, c := range cpus {
		if c > high {
			high = c
		}
	}
	if high < 0 {
		return []byte{}
	}
	mask := make([]byte, high/8+1)
	for _, c := range cpus {
		if c >= 0 {
			mask[c/8] |= 1 << (uint(c) % 8)
		}
	}
	return mask
}

func waitJob(ctx context.Context, start func(done chan string) (int, error)) error {
	done := make(chan string, 1)
	if _, err := start(done); err != nil {
		return err
	}
	select {
	case result := <-done:
		if result != "done" && result != "skipped" {
			return fmt.Errorf("job result %q", result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isNoSuchUnit(err error) bool {
	if err == nil {
		return false
	}
	var de godbus.Error
	if errors.As(err, &de) && de.Name == "org.freedesktop.systemd1.NoSuchUnit" {
		return true
	}
	return strings.Contains(err.Error(), "not loaded") ||
		strings.Contains(err.Error(), "NoSuchUnit")
}

// Close holds nothing to close: every call hangs up its own connection.
func (c *client) Close() error { return nil }
