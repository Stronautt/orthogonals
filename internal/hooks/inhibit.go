package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

// InhibitSleep takes a logind sleep inhibitor and blocks until signalled.
func InhibitSleep(vm string) error {
	conn, err := godbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect system bus: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if !conn.SupportsUnixFDs() {
		return errors.New("system bus did not negotiate unix fd passing — cannot hold a sleep inhibitor")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	var fd godbus.UnixFD
	err = obj.CallWithContext(ctx, "org.freedesktop.login1.Manager.Inhibit", 0,
		"sleep", "orthogonals", "VFIO VM "+vm+" is running", "block").Store(&fd)
	if err != nil {
		return fmt.Errorf("logind Inhibit: %w", err)
	}
	if fd < 0 {
		return errors.New("logind returned no inhibitor fd")
	}
	lock := os.NewFile(uintptr(fd), "inhibitor")
	defer func() { _ = lock.Close() }()

	sctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-sctx.Done()
	return nil
}
