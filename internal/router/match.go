package router

import (
	"fmt"
	"regexp"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// Match 判断抽取出的变量是否满足全部规则 (AND)。
// 规则为空时恒为 true。
func Match(rules []config.Rule, vars map[string]string) (bool, error) {
	for _, r := range rules {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return false, fmt.Errorf("rule %q invalid regex: %w", r.Var, err)
		}
		if !re.MatchString(vars[r.Var]) {
			return false, nil
		}
	}
	return true, nil
}
