package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestEnvironmentValuesAreLiteral(t *testing.T) {
	t.Setenv("NESTED_VALUE", "must-not-be-expanded")
	values := []string{"password$NESTED_VALUE", "password$" + "{NESTED_VALUE}", "password$" + "{ENV:NESTED_VALUE}"}
	references := []string{"$LITERAL_SECRET", "$" + "{LITERAL_SECRET}", "$" + "{ENV:LITERAL_SECRET}"}
	for _, value := range values {
		t.Setenv("LITERAL_SECRET", value)
		for _, reference := range references {
			if got := expandConfigEnv(reference, true); got != value {
				t.Errorf("reference %q changed literal value: got %q want %q", reference, got, value)
			}
		}
	}
	t.Setenv("LITERAL_SECRET", values[0])
	raw := "admin:\n  token: $" + "{LITERAL_SECRET}\nheartbeat:\n  targets:\n    - name: kuma\n      url: https://kuma.example/api/push/test\n      token: $" + "{LITERAL_SECRET}\nsources:\n  - name: logs\n    auth:\n      type: token\n      secret: $" + "{LITERAL_SECRET}\n    actions:\n      - type: exec\n        command: /bin/true\n"
	cfg, err := loadHeartbeatYAML(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Token != values[0] || cfg.Sources[0].Auth.Secret != values[0] || cfg.Heartbeat.Targets[0].Token != values[0] {
		t.Fatal("Load changed a credential containing a dollar sign")
	}
}

func TestHeartbeatValidationDoesNotExposeURLCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"https://kuma.example/FAKE-PUSH-SECRET/%zz?token=FAKE-QUERY-SECRET",
		"https://kuma.example/FAKE-PUSH-SECRET#fragment",
	} {
		_, err := loadHeartbeatYAML(t, "heartbeat:\n  targets:\n    - name: kuma\n      url: "+endpoint+"\n")
		if err == nil {
			t.Fatal("expected invalid target")
		}
		if strings.Contains(err.Error(), "FAKE-") {
			t.Fatalf("credential leaked in error: %v", err)
		}
	}
}

func TestRateLimitConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := loadHeartbeatYAML(t, "server:\n  rate_limit:\n    global:\n      burst: 500\n      per_second: 75\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.RateLimit.Global.Burst != 500 || cfg.Server.RateLimit.Global.PerSecond != 75 || cfg.Server.RateLimit.SourcePerIP.Burst != 30 {
		t.Fatalf("unexpected limits: %+v", cfg.Server.RateLimit)
	}
	for _, yaml := range []string{
		"server:\n  rate_limit:\n    global:\n      burst: -1\n",
		"server:\n  rate_limit:\n    pre_auth_per_ip:\n      per_second: .nan\n",
		"server:\n  rate_limit:\n    source_per_ip:\n      per_second: .inf\n",
	} {
		if _, err := loadHeartbeatYAML(t, yaml); err == nil {
			t.Fatalf("accepted invalid limits: %s", yaml)
		}
	}
}

func TestHeartbeatCardinalityAndHostBounds(t *testing.T) {
	var config strings.Builder
	config.WriteString("heartbeat:\n  targets:\n")
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&config, "    - name: target%d\n      url: https://example.com/push/test\n", i)
	}
	if _, err := loadHeartbeatYAML(t, config.String()); err == nil {
		t.Fatal("accepted too many targets")
	}
	if _, err := loadHeartbeatYAML(t, "heartbeat:\n  host: "+strings.Repeat("x", 129)+"\n"); err == nil {
		t.Fatal("accepted unbounded host")
	}
}
