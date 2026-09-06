package policy

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestAllow_Allowlist(t *testing.T) {
	src := &config.Source{
		Name: "logs",
		LogPolicy: &config.LogPolicyConfig{
			Mode:     "allowlist",
			Field:    "service",
			Services: []string{"edge-nginx"},
			OnReject: "drop_and_count",
		},
	}
	if _, ok := Allow(src, map[string]string{"service": "edge-nginx"}); !ok {
		t.Fatal("expected allow")
	}
	val, ok := Allow(src, map[string]string{"service": "postgres"})
	if ok {
		t.Fatal("expected reject")
	}
	if val != "postgres" {
		t.Fatalf("value=%q", val)
	}
}

func TestRedact(t *testing.T) {
	src := &config.Source{
		Redaction: &config.RedactionConfig{
			Patterns: []string{`sk-[A-Za-z0-9]{16,}`},
			Compiled: []*regexp.Regexp{regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`)},
		},
	}
	vars := map[string]string{"message": "token sk-abcdefghijklmnopqrstuvwxyz"}
	Redact(src, vars)
	if vars["message"] != "token ***" {
		t.Fatalf("message=%q", vars["message"])
	}
}

func TestCountersSummary(t *testing.T) {
	c := NewCounters("container-log")
	c.Inc("container-log")
	c.Inc("container-log")
	c.Inc("container-log")
	got := c.Summary()
	want := "dropped: container-log × 3"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCounters_BoundDimensionsAndSummary(t *testing.T) {
	c := NewCounters("logs", "invalid\nsource", strings.Repeat("x", 65))
	for i := 0; i < 2000; i++ {
		c.Inc(fmt.Sprintf("untrusted-%d-%s", i, strings.Repeat("x", 4096)))
		c.Inc("logs")
	}
	snapshot := c.Snapshot()
	if len(snapshot) != 2 || snapshot["logs"][ReasonAllowlistRejected] != 2000 || snapshot[UnknownSource][ReasonAllowlistRejected] != 2000 {
		t.Fatalf("unexpected bounded counters: %+v", snapshot)
	}
	for source, reasons := range snapshot {
		if len(reasons) != 1 {
			t.Fatalf("source %s has arbitrary rejection dimensions: %+v", source, reasons)
		}
	}
	snapshot["logs"][ReasonAllowlistRejected] = 0
	if c.Snapshot()["logs"][ReasonAllowlistRejected] != 2000 {
		t.Fatal("snapshot mutates original counters")
	}
	if summary := c.Summary(); strings.Contains(summary, "untrusted") || strings.Contains(summary, "\n") || len(summary) > 1024 {
		t.Fatalf("unsafe summary: %q", summary)
	}

	sources := make([]string, 100)
	for i := range sources {
		sources[i] = fmt.Sprintf("%064d", i)
	}
	c = NewCounters(sources...)
	for _, source := range sources {
		c.Inc(source)
	}
	if summary := c.Summary(); len(summary) > 1024 || !strings.HasSuffix(summary, "remaining sources × 90") {
		t.Fatalf("summary must aggregate omitted source counts: %q", summary)
	}
}

func TestCounters_ConcurrentIncrements(t *testing.T) {
	c := NewCounters("logs")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				c.Inc("logs")
				_ = c.Snapshot()
				_ = c.Summary()
			}
		})
	}
	wg.Wait()
	if got := c.Snapshot()["logs"][ReasonAllowlistRejected]; got != 800 {
		t.Fatalf("concurrent count=%d want=800", got)
	}
}
