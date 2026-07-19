package handler

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

const maxExecOutput = 8 << 10 // 8 KiB runtime + stored detail cap

// Exec 执行外部脚本/命令。
//
// 安全约束:
//   - 不经过 shell, 命令与参数逐个传入 exec.Command, 避免命令注入;
//   - 强制超时; Linux 下用独立进程组终止整棵子树;
//   - 输出经有界 writer, 限制运行期内存;
//   - 仅向子进程传递精简环境变量, 不继承全部父进程环境。
type Exec struct {
	defaultTimeout time.Duration
}

func NewExec() *Exec {
	return &Exec{defaultTimeout: 60 * time.Second}
}

func (h *Exec) Type() string { return "exec" }

func (h *Exec) Handle(ctx context.Context, ac config.ActionConfig, vars map[string]string) (Result, error) {
	secretFields := append([]string{ac.Command}, ac.Args...)
	if missing := missingExplicitSecret(secretFields...); missing != "" {
		return Result{}, fmt.Errorf("%w: required action secret %s is not set", ErrNonRetryable, missing)
	}
	if ac.Command == "" {
		return Result{}, fmt.Errorf("exec: empty command")
	}
	timeout := ac.Timeout.Duration
	if timeout <= 0 {
		timeout = h.defaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := make([]string, len(ac.Args))
	for i, a := range ac.Args {
		args[i] = interpolate(a, vars)
	}
	cmd := exec.Command(ac.Command, args...)
	cmd.Env = minimalExecEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := &cappedWriter{limit: maxExecOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return Result{Target: ac.Command}, fmt.Errorf("exec start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-cctx.Done():
		killProcessGroup(cmd)
		<-done
		return Result{Target: ac.Command, Detail: out.String()}, fmt.Errorf("exec timeout after %s", timeout)
	case err := <-done:
		if err != nil {
			return Result{Target: ac.Command, Detail: out.String()}, fmt.Errorf("exec failed: %w", err)
		}
		return Result{Target: ac.Command, Detail: out.String()}, nil
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func minimalExecEnv() []string {
	keys := []string{"PATH", "LANG", "LC_ALL", "HOME", "TMPDIR", "TZ"}
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if len(env) == 0 {
		env = []string{"PATH=/usr/bin:/bin"}
	}
	return env
}

// cappedWriter keeps at most limit bytes; further writes are discarded.
type cappedWriter struct {
	limit int
	buf   []byte
	n     int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	orig := len(p)
	w.n += orig
	remain := w.limit - len(w.buf)
	if remain <= 0 {
		return orig, nil
	}
	if len(p) > remain {
		p = p[:remain]
	}
	w.buf = append(w.buf, p...)
	return orig, nil
}

func (w *cappedWriter) String() string {
	if w.n > w.limit {
		return string(w.buf) + "...(truncated)"
	}
	return string(w.buf)
}

var _ io.Writer = (*cappedWriter)(nil)
