package policy

import (
	"regexp"
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
	c := NewCounters()
	c.Inc("container-log", "postgres")
	c.Inc("container-log", "postgres")
	c.Inc("container-log", "foo")
	got := c.Summary()
	want := "dropped: container-log × 3 (foo, postgres)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
