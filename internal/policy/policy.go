// Package policy implements optional per-source egress allowlists and redaction.
package policy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// Counters tracks allowlist rejections for heartbeat /metrics.
type Counters struct {
	mu   sync.Mutex
	data map[string]map[string]*atomic.Uint64 // source -> value -> count
}

func NewCounters() *Counters {
	return &Counters{data: map[string]map[string]*atomic.Uint64{}}
}

func (c *Counters) Inc(source, value string) {
	if c == nil {
		return
	}
	if value == "" {
		value = "(empty)"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byVal, ok := c.data[source]
	if !ok {
		byVal = map[string]*atomic.Uint64{}
		c.data[source] = byVal
	}
	ctr, ok := byVal[value]
	if !ok {
		ctr = &atomic.Uint64{}
		byVal[value] = ctr
	}
	ctr.Add(1)
}

// Snapshot returns source -> value -> count.
func (c *Counters) Snapshot() map[string]map[string]uint64 {
	out := map[string]map[string]uint64{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for src, byVal := range c.data {
		inner := map[string]uint64{}
		for v, ctr := range byVal {
			inner[v] = ctr.Load()
		}
		out[src] = inner
	}
	return out
}

// Summary formats drop counters for heartbeats, e.g.
// "dropped: container-log × 37 (foo, postgres)".
func (c *Counters) Summary() string {
	snap := c.Snapshot()
	if len(snap) == 0 {
		return "dropped: none"
	}
	sources := make([]string, 0, len(snap))
	for s := range snap {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	parts := make([]string, 0, len(sources))
	for _, src := range sources {
		byVal := snap[src]
		var total uint64
		vals := make([]string, 0, len(byVal))
		for v, n := range byVal {
			total += n
			vals = append(vals, v)
		}
		sort.Strings(vals)
		parts = append(parts, fmt.Sprintf("%s × %d (%s)", src, total, strings.Join(vals, ", ")))
	}
	return "dropped: " + strings.Join(parts, "; ")
}

// Allow reports whether vars pass the source log_policy allowlist.
// ok=false means reject (caller should skip + count).
func Allow(src *config.Source, vars map[string]string) (value string, ok bool) {
	if src == nil || src.LogPolicy == nil {
		return "", true
	}
	lp := src.LogPolicy
	field := lp.Field
	if field == "" {
		field = "service"
	}
	value = vars[field]
	for _, allowed := range lp.Services {
		if value == allowed {
			return value, true
		}
	}
	return value, false
}

// Redact applies compiled redaction patterns to all var values in place.
func Redact(src *config.Source, vars map[string]string) {
	if src == nil || src.Redaction == nil || len(src.Redaction.Compiled) == 0 {
		return
	}
	for key, val := range vars {
		redacted := val
		for _, re := range src.Redaction.Compiled {
			redacted = re.ReplaceAllString(redacted, "***")
		}
		vars[key] = redacted
	}
}
