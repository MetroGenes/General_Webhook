package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestExec_CapsOutputMemory(t *testing.T) {
	script := filepath.Join(t.TempDir(), "big.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npython3 -c 'print(\"x\"*200000)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewExec()
	res, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "exec", Command: script, Timeout: config.Duration{Duration: 5 * time.Second},
	}, nil)
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

func TestExec_KillsProcessGroupOnTimeout(t *testing.T) {
	script := filepath.Join(t.TempDir(), "bg.sh")
	marker := filepath.Join(t.TempDir(), "alive")
	content := "#!/bin/sh\n(sleep 30; touch '" + marker + "') &\nsleep 30\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewExec()
	start := time.Now()
	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "exec", Command: script, Timeout: config.Duration{Duration: 50 * time.Millisecond},
	}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed %s, timeout should kill process group quickly", elapsed)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child should have been killed")
	}
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
