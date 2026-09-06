package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestExec_CapsOutputMemory(t *testing.T) {
	h := NewExec()
	res, err := h.Handle(context.Background(), execHelperAction(t, "output"), nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(res.Detail) > maxExecOutput+20 {
		t.Fatalf("detail len=%d exceeds cap", len(res.Detail))
	}
	if !strings.Contains(res.Detail, "truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Detail[:min(80, len(res.Detail))])
	}
}

func TestExec_KillsProcessGroupOnCancellation(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	marker := filepath.Join(t.TempDir(), "alive")
	cleanupExecHelper(t, pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ac := execHelperAction(t, "group", pidFile, marker)
	done := make(chan error, 1)
	go func() {
		_, err := NewExec().Handle(ctx, ac, nil)
		done <- err
	}()
	waitExecHelperPID(t, pidFile)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want cancellation error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process group did not exit after cancellation")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child should have been killed")
	}
}

func TestExec_DetachedChildCannotHoldWaitOpen(t *testing.T) {
	for _, mode := range []string{"cancellation", "timeout", "parent-exits"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			cleanupExecHelper(t, pidFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			kind := "detached"
			if mode == "parent-exits" {
				kind = "detached-exit"
			}
			ac := execHelperAction(t, kind, pidFile)
			if mode == "timeout" {
				ac.Timeout.Duration = 2 * time.Second
			}
			done := make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := NewExec().Handle(ctx, ac, nil)
				done <- err
			}()
			pid := waitExecHelperPID(t, pidFile)
			if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
				t.Fatalf("helper must escape to its own session: pgid=%d pid=%d err=%v", pgid, pid, err)
			}
			if mode == "cancellation" {
				started = time.Now()
				cancel()
			}
			select {
			case err := <-done:
				want := error(context.Canceled)
				if mode == "timeout" {
					want = context.DeadlineExceeded
				} else if mode == "parent-exits" {
					want = exec.ErrWaitDelay
				}
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("detached stdout holder blocked the handler")
			}
			limit := 2 * time.Second
			if mode == "timeout" || mode == "parent-exits" {
				limit = 4 * time.Second
			}
			if elapsed := time.Since(started); elapsed > limit {
				t.Fatalf("handler exceeded bounded wait: %s > %s", elapsed, limit)
			}
			// Wait must have returned while the detached process was still alive,
			// proving output-pipe closure rather than natural child termination.
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("detached helper exited unexpectedly: %v", err)
			}
		})
	}
}

const execHelperMarker = "--general-webhook-exec-helper"

func execHelperArgs(args ...string) []string {
	return append([]string{"-test.run=^TestExecHelperProcess$", "--", execHelperMarker}, args...)
}

func execHelperAction(t *testing.T, args ...string) config.ActionConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.ActionConfig{
		Type: "exec", Command: executable, Args: execHelperArgs(args...),
		Timeout: config.Duration{Duration: 5 * time.Second},
	}
}

// TestExecHelperProcess uses the Go test binary as a subprocess so the suite
// needs no Python, shell utilities, or extra packages in the Alpine builder.
func TestExecHelperProcess(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == execHelperMarker {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "output":
		fmt.Print(strings.Repeat("x", 200000))
	case "leaf":
		if len(args) > 1 && args[1] != "" {
			time.Sleep(300 * time.Millisecond)
			if err := os.WriteFile(args[1], []byte("alive"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		time.Sleep(30 * time.Second)
	case "group", "detached", "detached-exit":
		executable, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		marker := ""
		if len(args) > 2 {
			marker = args[2]
		}
		child := exec.Command(executable, execHelperArgs("leaf", marker)...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if args[0] != "group" {
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if args[0] != "detached-exit" {
			time.Sleep(30 * time.Second)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func waitExecHelperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(raw))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not start its child")
	return 0
}

func cleanupExecHelper(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(raw))
		if err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestCappedWriter(t *testing.T) {
	w := &cappedWriter{limit: 8}
	n, err := w.Write([]byte("abcdefghijklmnop"))
	if err != nil || n != 16 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if w.String() != "abcdefgh...(truncated)" {
		t.Fatalf("got %q", w.String())
	}
}
