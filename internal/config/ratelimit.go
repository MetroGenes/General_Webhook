package config

import (
	"fmt"
	"math"
)

// RateLimitConfig controls one token bucket. Omitted/zero values use defaults.
type RateLimitConfig struct {
	Burst     int     `yaml:"burst"`
	PerSecond float64 `yaml:"per_second"`
}

// RateLimitsConfig separates unauthenticated IP traffic from accepted sources.
type RateLimitsConfig struct {
	PreAuthPerIP RateLimitConfig `yaml:"pre_auth_per_ip"`
	Global       RateLimitConfig `yaml:"global"`
	SourcePerIP  RateLimitConfig `yaml:"source_per_ip"`
	AdminPerIP   RateLimitConfig `yaml:"admin_per_ip"`
}

func (c RateLimitsConfig) WithDefaults() RateLimitsConfig {
	defaults := []RateLimitConfig{{120, 20}, {120, 2}, {30, 0.5}, {60, 1}}
	fields := []*RateLimitConfig{&c.PreAuthPerIP, &c.Global, &c.SourcePerIP, &c.AdminPerIP}
	for i, field := range fields {
		if field.Burst == 0 {
			field.Burst = defaults[i].Burst
		}
		if field.PerSecond == 0 {
			field.PerSecond = defaults[i].PerSecond
		}
	}
	return c
}

func (c RateLimitsConfig) validate() error {
	fields := []struct {
		name  string
		value RateLimitConfig
	}{
		{"pre_auth_per_ip", c.PreAuthPerIP}, {"global", c.Global},
		{"source_per_ip", c.SourcePerIP}, {"admin_per_ip", c.AdminPerIP},
	}
	for _, field := range fields {
		if field.value.Burst < 1 || field.value.Burst > 100000 {
			return fmt.Errorf("server.rate_limit.%s.burst must be 1..100000", field.name)
		}
		rate := field.value.PerSecond
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0.01 || rate > 100000 {
			return fmt.Errorf("server.rate_limit.%s.per_second must be 0.01..100000", field.name)
		}
	}
	return nil
}
