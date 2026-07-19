package router

import (
	"testing"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestMatch(t *testing.T) {
	vars := map[string]string{"branch": "refs/heads/main"}
	ok, err := Match([]config.Rule{{Var: "branch", Regex: `^refs/heads/(main|master)$`}}, vars)
	if err != nil || !ok {
		t.Fatalf("matched=%v err=%v", ok, err)
	}
	ok, err = Match([]config.Rule{{Var: "branch", Regex: `^release/`}}, vars)
	if err != nil || ok {
		t.Fatalf("matched=%v err=%v", ok, err)
	}
	if _, err := Match([]config.Rule{{Var: "branch", Regex: `(`}}, vars); err == nil {
		t.Fatal("invalid regex should fail")
	}
}
