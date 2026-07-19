package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var explicitSecretRefRe = regexp.MustCompile(`\$\{secret:([A-Za-z_][A-Za-z0-9_]*)\}`)

// SourceSnapshot is the durable execution plan captured at accept time.
type SourceSnapshot struct {
	Name    string            `json:"name"`
	Extract map[string]string `json:"extract,omitempty"`
	Rules   []Rule            `json:"rules,omitempty"`
	Actions []ActionConfig    `json:"actions"`
}

// SnapshotSource serializes extract/rules/actions for durable outbox execution.
func SnapshotSource(src *Source) (actionsJSON string, err error) {
	if src == nil {
		return "", fmt.Errorf("nil source")
	}
	snap := SourceSnapshot{
		Name:    src.Name,
		Extract: src.Extract,
		Rules:   src.Rules,
		Actions: src.Actions,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseSourceSnapshot restores a Source from durable JSON.
func ParseSourceSnapshot(raw string) (*Source, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty snapshot")
	}
	var snap SourceSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, err
	}
	return &Source{
		Name:    snap.Name,
		Extract: snap.Extract,
		Rules:   snap.Rules,
		Actions: snap.Actions,
	}, nil
}

// ConfigHash returns a stable hash of all sources' execution plans (excludes secrets).
func ConfigHash(c *Config) string {
	if c == nil {
		return ""
	}
	type plan struct {
		Name    string            `json:"name"`
		Extract map[string]string `json:"extract,omitempty"`
		Rules   []Rule            `json:"rules,omitempty"`
		Actions []ActionConfig    `json:"actions"`
	}
	plans := make([]plan, 0, len(c.Sources))
	for _, s := range c.Sources {
		plans = append(plans, plan{
			Name:    s.Name,
			Extract: s.Extract,
			Rules:   s.Rules,
			Actions: s.Actions,
		})
	}
	b, err := json.Marshal(plans)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ActionSecretReplacements returns currently available secret values mapped to
// their durable references. Values are used only in memory during migration.
func ActionSecretReplacements(c *Config) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	var names []string
	seen := map[string]bool{}
	for _, source := range c.Sources {
		for _, action := range source.Actions {
			for _, value := range actionStrings(action) {
				for _, match := range explicitSecretRefRe.FindAllStringSubmatch(value, -1) {
					if !seen[match[1]] {
						seen[match[1]] = true
						names = append(names, match[1])
					}
				}
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			if _, exists := out[value]; !exists {
				out[value] = "${secret:" + name + "}"
			}
		}
	}
	return out
}

// ScrubSnapshotSecrets replaces credentials embedded by older releases with
// runtime references while preserving the rest of the execution plan.
func ScrubSnapshotSecrets(raw string, replacements map[string]string) (string, error) {
	if len(replacements) == 0 || raw == "" {
		return raw, nil
	}
	source, err := ParseSourceSnapshot(raw)
	if err != nil {
		return "", err
	}
	values := make([]string, 0, len(replacements))
	for value := range replacements {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	replace := func(value string) string {
		for _, secret := range values {
			value = strings.ReplaceAll(value, secret, replacements[secret])
		}
		return value
	}
	for i := range source.Actions {
		action := &source.Actions[i]
		action.URL = replace(action.URL)
		action.Command = replace(action.Command)
		action.Body = replace(action.Body)
		for j := range action.Args {
			action.Args[j] = replace(action.Args[j])
		}
		headers := make(map[string]string, len(action.Headers))
		for key, value := range action.Headers {
			headers[replace(key)] = replace(value)
		}
		action.Headers = headers
	}
	return SnapshotSource(source)
}

func actionStrings(action ActionConfig) []string {
	values := []string{action.URL, action.Command, action.Body}
	values = append(values, action.Args...)
	for key, value := range action.Headers {
		values = append(values, key, value)
	}
	return values
}
