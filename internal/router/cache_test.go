package router

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestRegexpCache_ReusesPatternsAndBoundsHistoricalEntries(t *testing.T) {
	var cache regexpCache
	first, err := cache.compile("^service-0$")
	if err != nil {
		t.Fatal(err)
	}
	again, err := cache.compile("^service-0$")
	if err != nil || first != again {
		t.Fatal("repeated pattern compiled again")
	}
	for i := 1; i < maxCachedRules+20; i++ {
		if _, err := cache.compile(fmt.Sprintf("^service-%d$", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != maxCachedRules || cache.entries["^service-0$"] != nil {
		t.Fatal("historical patterns were retained beyond the cache bound")
	}
	if re, err := cache.compile("^service-0$"); err != nil || !re.MatchString("service-0") {
		t.Fatalf("evicted rule no longer works: %v", err)
	}
}

func TestRegexpCache_LargeAndInvalidPatternsAreNotRetained(t *testing.T) {
	var cache regexpCache
	large := strings.Repeat("a", maxCachedPatternBytes+1)
	if re, err := cache.compile(large); err != nil || !re.MatchString(large) {
		t.Fatalf("large rule lost compatibility: %v", err)
	}
	if _, err := cache.compile("("); err == nil {
		t.Fatal("invalid expression accepted")
	}
	if len(cache.entries) != 0 {
		t.Fatal("large or invalid expression entered cache")
	}
}

func TestRegexpCache_ConcurrentMatchesUseCurrentVariables(t *testing.T) {
	var cache regexpCache
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Go(func() {
			value := fmt.Sprintf("service-%d", worker%8)
			for range 100 {
				re, err := cache.compile("^" + value + "$")
				if err != nil || !re.MatchString(value) || re.MatchString("unexpected") {
					t.Errorf("concurrent rule lookup failed: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if len(cache.entries) != 8 {
		t.Fatalf("cached %d patterns, want 8", len(cache.entries))
	}
	rules := []config.Rule{{Var: "service", Regex: "^allowed$"}}
	if ok, err := Match(rules, map[string]string{"service": "allowed"}); err != nil || !ok {
		t.Fatalf("allowed vars rejected: %v", err)
	}
	if ok, err := Match(rules, map[string]string{"service": "denied"}); err != nil || ok {
		t.Fatalf("cached expression reused previous event result: ok=%v err=%v", ok, err)
	}
}
