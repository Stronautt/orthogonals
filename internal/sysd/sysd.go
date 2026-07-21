// Package sysd is the systemd seam over the manager's private D-Bus socket.
package sysd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// Client is what orthogonals needs from systemd.
type Client interface {
	// EnableUnit and DisableUnit change unit-file symlinks and reload the manager.
	EnableUnit(unit string) error
	DisableUnit(unit string) error
	// UnitFileState reports enabled/disabled/static/…; errors degrade to "unknown".
	UnitFileState(unit string) string
	Reload() error
	RestartUnit(unit string) error
	TryRestartUnit(unit string) error
	// StopUnit stops a unit; an absent unit is not an error.
	StopUnit(unit string) error
	// ResetFailedUnit clears a unit's failed state; an absent unit is not an error.
	ResetFailedUnit(unit string) error
	// StartTransientUnit runs argv as a transient systemd service.
	StartTransientUnit(name string, argv []string) error
	Close() error
}

// New returns a lazily connecting client for the local systemd manager.
func New() Client { return &client{} }

// callTimeout bounds every manager call.
const callTimeout = time.Minute

type client struct {
	c *sddbus.Conn
}

func (c *client) ensure(ctx context.Context) (*sddbus.Conn, error) {
	if c.c != nil {
		return c.c, nil
	}
	conn, err := sddbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to systemd (root required): %w", err)
	}
	c.c = conn
	return conn, nil
}

func (c *client) do(op string, f func(ctx context.Context, conn *sddbus.Conn) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	conn, err := c.ensure(ctx)
	if err != nil {
		return err
	}
	err = f(ctx, conn)
	if isClosedConn(err) {
		conn.Close()
		c.c = nil
		if conn, err2 := c.ensure(ctx); err2 == nil {
			err = f(ctx, conn)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// isClosedConn matches the shapes a dropped private connection surfaces as.
func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, godbus.ErrClosed) || errors.Is(err, net.ErrClosed) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection closed by user")
}

func (c *client) EnableUnit(unit string) error {
	err := c.do("enable unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		if _, _, err := conn.EnableUnitFilesContext(ctx, []string{unit}, false, false); err != nil {
			return err
		}
		return conn.ReloadContext(ctx)
	})
	c.resetConn()
	return err
}

func (c *client) DisableUnit(unit string) error {
	err := c.do("disable unit "+unit, func(ctx context.Context, conn *sddbus.Conn) error {
		if _, err := conn.DisableUnitFilesContext(ctx, []string{unit}, false); err != nil {
			return err
		}
		return conn.ReloadContext(ctx)
	})
	c.resetConn()
	return err
}

// resetConn drops the connection so the next call dials fresh.
func (c *client) resetConn() {
	if c.c != nil {
		c.c.Close()
		c.c = nil
	}
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
	err := c.do("daemon-reload", func(ctx context.Context, conn *sddbus.Conn) error {
		return conn.ReloadContext(ctx)
	})
	c.resetConn()
	return err
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

// StartTransientUnit runs argv as a transient .service.
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

// waitJob starts a systemd job and blocks on its completion signal.
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

// isNoSuchUnit reports the systemd "no such unit" error.
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

func (c *client) Close() error {
	if c.c == nil {
		return nil
	}
	c.c.Close()
	c.c = nil
	return nil
}
