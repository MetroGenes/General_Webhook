// Package policy implements optional per-source egress allowlists and redaction.
package policy

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

const (
	ReasonAllowlistRejected = "allowlist_rejected"
	UnknownSource           = "other"
	maxSummarySources       = 10
)

// Counters tracks committed allowlist rejections for heartbeat /metrics. Its
// dimensions are fixed at construction; event values never become map keys.
type Counters struct {
	mu      sync.Mutex
	data    map[string]uint64
	sources []string
}

func NewCounters(sources ...string) *Counters {
	c := &Counters{data: map[string]uint64{UnknownSource: 0}}
	for _, source := range sources {
		if validSourceName(source) {
			c.data[source] = 0
		}
	}
	for source := range c.data {
		c.sources = append(c.sources, source)
	}
	sort.Strings(c.sources)
	return c
}

func (c *Counters) Inc(source string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[source]; !ok {
		source = UnknownSource
	}
	c.data[source]++
}

// Snapshot returns source -> fixed reason -> count. Sources removed from the
// active configuration, including old durable snapshots, share the other bucket.
func (c *Counters) Snapshot() map[string]map[string]uint64 {
	out := map[string]map[string]uint64{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for src, count := range c.data {
		if count > 0 {
			out[src] = map[string]uint64{ReasonAllowlistRejected: count}
		}
	}
	return out
}

// Summary includes at most ten configured source names, each limited to 64
// ASCII bytes. The remaining counts are aggregated, keeping it below 1 KiB
// regardless of the number or size of rejected events or configured sources.
func (c *Counters) Summary() string {
	if c == nil {
		return "dropped: none"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, maxSummarySources+1)
	var rest uint64
	for _, src := range c.sources {
		count := c.data[src]
		if count == 0 {
			continue
		}
		if len(parts) < maxSummarySources {
			parts = append(parts, fmt.Sprintf("%s × %d", src, count))
		} else {
			rest += count
		}
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf("remaining sources × %d", rest))
	}
	if len(parts) == 0 {
		return "dropped: none"
	}
	return "dropped: " + strings.Join(parts, "; ")
}

func validSourceName(source string) bool {
	if len(source) == 0 || len(source) > 64 {
		return false
	}
	for _, c := range source {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
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
