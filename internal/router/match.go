package router

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

const (
	maxCachedRules        = 256
	maxCachedPatternBytes = 4 << 10
)

var compiledRules regexpCache

// regexpCache is a bounded FIFO of compiled patterns shared by current config
// and durable snapshots. Large expressions still work, but cannot occupy the
// cache. Only compilation is cached; matches always use the current event vars.
type regexpCache struct {
	mu      sync.Mutex
	entries map[string]*regexp.Regexp
	order   [maxCachedRules]string
	next    int
}

func (c *regexpCache) compile(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > maxCachedPatternBytes {
		return regexp.Compile(pattern)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if re := c.entries[pattern]; re != nil {
		return re, nil
	}
	// Compile while locked to avoid many workers compiling the same pattern
	// on first use. Cached patterns have a strict input-size bound.
	pattern = strings.Clone(pattern)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[string]*regexp.Regexp, maxCachedRules)
	}
	if len(c.entries) == maxCachedRules {
		delete(c.entries, c.order[c.next])
	}
	c.entries[pattern] = re
	c.order[c.next] = pattern
	c.next = (c.next + 1) % maxCachedRules
	return re, nil
}

// Match 判断抽取出的变量是否满足全部规则 (AND)。
// 规则为空时恒为 true。
func Match(rules []config.Rule, vars map[string]string) (bool, error) {
	for _, r := range rules {
		re, err := compiledRules.compile(r.Regex)
		if err != nil {
			return false, fmt.Errorf("rule %q invalid regex: %w", r.Var, err)
		}
		if !re.MatchString(vars[r.Var]) {
			return false, nil
		}
	}
	return true, nil
}
