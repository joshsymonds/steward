package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	notifydExitHelperEnv          = "STEWARD_NOTIFYD_EXIT_HELPER"
	notifydExitHelperModeEnv      = "STEWARD_NOTIFYD_EXIT_HELPER_MODE"
	notifydExitParentMarkerEnv    = "STEWARD_NOTIFYD_EXIT_PARENT_MARKER"
	notifydExitHelperBlockMode    = "block"
	notifydExitHelperEnvCheckMode = "environment-check"
)

func TestNotifydExitHelper(_ *testing.T) {
	if os.Getenv(notifydExitHelperEnv) != "1" {
		return
	}
	if os.Getenv(notifydExitParentMarkerEnv) != "" {
		_, _ = fmt.Fprintln(os.Stderr, "test parent environment marker reached notifyd child")
		os.Exit(2)
	}
	switch os.Getenv(notifydExitHelperModeEnv) {
	case notifydExitHelperBlockMode:
		select {}
	case notifydExitHelperEnvCheckMode:
		return
	}
	copy(os.Args, []string{"steward", "notifyd", "--dry-run", "--state-base", os.Getenv("XDG_STATE_HOME")})
	runNotifydCommand()
}

func TestNotifydFatalServeErrorExitsNonzeroAndRemovesSocket(t *testing.T) {
	child := startNotifydChild(t, "")
	sockPath := waitForNotifydSocket(t, child)

	var limit unix.Rlimit
	if err := unix.Prlimit(child.cmd.Process.Pid, unix.RLIMIT_NOFILE, nil, &limit); err != nil {
		child.stop(t)
		t.Fatalf("read child RLIMIT_NOFILE: %v", err)
	}
	limit.Cur = 0
	if err := unix.Prlimit(child.cmd.Process.Pid, unix.RLIMIT_NOFILE, &limit, nil); err != nil {
		child.stop(t)
		t.Fatalf("lower child RLIMIT_NOFILE: %v", err)
	}

	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		child.stop(t)
		t.Fatalf("connect to notifyd: %v", err)
	}
	_ = conn.Close()

	exitErr := child.wait(t, 5*time.Second)
	if exitErr == nil {
		t.Fatalf("fatal Serve error exited zero; stderr: %s", child.stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(exitErr, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("fatal Serve error = %v, want nonzero exit", exitErr)
	}
	diagnostic := child.stderr.String()
	if !strings.Contains(diagnostic, "accept") ||
		!strings.Contains(diagnostic, "too many open files") {
		t.Fatalf("stderr = %q, want fatal accept diagnostic", diagnostic)
	}
	if _, statErr := os.Lstat(sockPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("socket remains after fatal exit: %v", statErr)
	}
}

func TestNotifydSIGTERMExitsZeroAndRemovesSocket(t *testing.T) {
	child := startNotifydChild(t, "")
	sockPath := waitForNotifydSocket(t, child)
	if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		child.stop(t)
		t.Fatalf("signal notifyd: %v", err)
	}
	if err := child.wait(t, 5*time.Second); err != nil {
		t.Fatalf("SIGTERM exit = %v, want zero; stderr: %s", err, child.stderr.String())
	}
	if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after SIGTERM: %v", err)
	}
}

func TestNotifydChildEnvironmentIsolated(t *testing.T) {
	child := startNotifydChild(t, notifydExitHelperEnvCheckMode)
	if err := child.wait(t, 5*time.Second); err != nil {
		t.Fatalf("environment-isolation helper exit = %v; stderr: %s", err, child.stderr.String())
	}
}

func TestNotifydChildTimeoutKillsAndReaps(t *testing.T) {
	child := startNotifydChild(t, notifydExitHelperBlockMode)
	if !child.waitFor(t, 100*time.Millisecond) {
		t.Fatal("non-exiting helper exited before cleanup deadline")
	}
	if !child.reaped {
		t.Fatal("non-exiting helper was not reaped after deadline cleanup")
	}
}

type notifydChild struct {
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	runtime string
	done    chan struct{}
	waitErr error
	reaped  bool
}

func makeNotifydRuntimeDir() (string, error) {
	return os.MkdirTemp("/tmp", "steward-q2-")
}

func startNotifydChild(t *testing.T, mode string) *notifydChild {
	t.Helper()
	runtime, err := makeNotifydRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })

	home := t.TempDir()
	state := t.TempDir()
	t.Setenv(notifydExitParentMarkerEnv, "parent-only")

	child := &notifydChild{runtime: runtime, done: make(chan struct{})}
	child.cmd = exec.Command(os.Args[0], "-test.run=^TestNotifydExitHelper$", "arg2", "arg3", "arg4")
	child.cmd.Env = []string{
		notifydExitHelperEnv + "=1",
		notifydExitHelperModeEnv + "=" + mode,
		"HOME=" + home,
		"XDG_RUNTIME_DIR=" + runtime,
		"XDG_STATE_HOME=" + state,
		"STEWARD_NTFY_DISABLED=true",
		"LISTEN_FDS=",
		"LISTEN_PID=",
	}
	child.cmd.Stderr = &child.stderr
	if startErr := child.cmd.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	go func() {
		child.waitErr = child.cmd.Wait()
		child.reaped = true
		close(child.done)
	}()
	t.Cleanup(func() { child.stop(t) })
	return child
}

func waitForNotifydSocket(t *testing.T, child *notifydChild) string {
	t.Helper()
	path := filepath.Join(child.runtime, "steward", "notifyd.sock")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return path
		}
		select {
		case <-child.done:
			t.Fatalf(
				"notifyd exited before socket appeared at %s: %v; stderr: %s",
				path, child.waitErr, child.stderr.String(),
			)
		case <-deadline.C:
			child.stop(t)
			t.Fatalf("notifyd socket did not appear at %s; stderr: %s", path, child.stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (child *notifydChild) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	if child.waitFor(t, timeout) {
		t.Fatalf("notifyd child did not exit within %s", timeout)
	}
	return child.waitErr
}

// waitFor returns true only when it had to kill and reap a child that missed
// the deadline. stderr is read by callers only after this method returns.
func (child *notifydChild) waitFor(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-child.done:
		return false
	case <-time.After(timeout):
		child.stop(t)
		return true
	}
}

func (child *notifydChild) stop(t *testing.T) {
	t.Helper()
	select {
	case <-child.done:
		return
	default:
	}
	_ = child.cmd.Process.Kill()
	select {
	case <-child.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("notifyd child did not reap within cleanup deadline")
	}
}
