//go:build process_integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type runningApp struct {
	cmd     *exec.Cmd
	done    chan error
	log     *os.File
	logPath string
}

func startApp(t *testing.T, binary, configPath, logPath string) *runningApp {
	t.Helper()
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-config", configPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	app := &runningApp{cmd: cmd, done: make(chan error, 1), log: log, logPath: logPath}
	go func() { app.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Log(string(data))
		}
	})
	return app
}

func httpRequest(t *testing.T, method, endpoint, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Token", "review-source-token-0123456789")
	req.Header.Set("Authorization", "Bearer review-admin-token-0123456789")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func waitFor(t *testing.T, limit time.Duration, label string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", label)
}

func appFixture(t *testing.T, heartbeatURL string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	once := filepath.Join(dir, "first-execution")
	script := filepath.Join(dir, "action.sh")
	scriptBody := "#!/bin/sh\nif [ ! -f " + strconv.Quote(once) + " ]; then\n  echo $$ > " + strconv.Quote(once) + "\n  sleep 65\nfi\nprintf 'completed\\n'\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0700); err != nil {
		t.Fatal(err)
	}
	hb := ""
	if heartbeatURL != "" {
		hb = fmt.Sprintf("heartbeat:\n  interval: 10s\n  timeout: 1s\n  host: review-host\n  targets:\n    - name: local-monitor\n      url: %q\n      allow_http: true\n      mode: json\n", heartbeatURL)
	}
	cfg := fmt.Sprintf("server:\n  addr: %q\nadmin:\n  token: review-admin-token-0123456789\nstore:\n  path: %q\n  retain_payload: false\nqueue:\n  workers: 1\n  max_retries: 2\n%s"+"sources:\n  - name: local\n    auth:\n      type: token\n      secret: review-source-token-0123456789\n    extract:\n      job: $.job\n    actions:\n      - type: exec\n        command: %q\n        timeout: 90s\n", addr, filepath.Join(dir, "data", "review.db"), hb, script)
	configPath := filepath.Join(dir, "review.yaml")
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	return configPath, "http://" + addr, once
}

func acceptSlowEvent(t *testing.T, base, once string) string {
	t.Helper()
	waitFor(t, 5*time.Second, "readyz", func() bool { code, _ := httpRequest(t, http.MethodGet, base+"/readyz", ""); return code == 200 })
	code, body := httpRequest(t, http.MethodPost, base+"/webhook/local", `{"job":"review"}`)
	if code != 202 {
		t.Fatalf("accept code=%d body=%s", code, body)
	}
	var accepted struct {
		ID string `json:"event_id"`
	}
	if err := json.Unmarshal([]byte(body), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("accepted=%s err=%v", body, err)
	}
	waitFor(t, 5*time.Second, "first action running", func() bool {
		data, err := os.ReadFile(once)
		return err == nil && len(strings.TrimSpace(string(data))) > 0
	})
	return accepted.ID
}

func eventDone(t *testing.T, base, id string) bool {
	t.Helper()
	code, body := httpRequest(t, http.MethodGet, base+"/events/"+id, "")
	var event map[string]json.RawMessage
	if code != 200 || json.Unmarshal([]byte(body), &event) != nil {
		return false
	}
	var status string
	json.Unmarshal(event["status"], &status)
	if status == "" {
		var nested struct {
			Status string `json:"status"`
		}
		json.Unmarshal(event["event"], &nested)
		status = nested.Status
	}
	return status == "done"
}

func TestReviewCrashRestartPreservesAcceptedEvent(t *testing.T) {
	t.Parallel()
	binary := os.Getenv("REVIEW_BINARY")
	if binary == "" {
		t.Fatal("REVIEW_BINARY required")
	}
	configPath, base, once := appFixture(t, "")
	app := startApp(t, binary, configPath, filepath.Join(filepath.Dir(configPath), "before-crash.log"))
	id := acceptSlowEvent(t, base, once)
	if err := app.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-app.done:
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not stop process")
	}
	if raw, err := os.ReadFile(once); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	app2 := startApp(t, binary, configPath, filepath.Join(filepath.Dir(configPath), "after-crash.log"))
	waitFor(t, 8*time.Second, "done after crash recovery", func() bool { return eventDone(t, base, id) })
	app2.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-app2.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle shutdown timed out")
	}
	t.Logf("accepted event %s recovered after SIGKILL and completed on restart", id)
}

func TestReviewGracefulShutdownRequeuesAndStopsHeartbeat(t *testing.T) {
	t.Parallel()
	binary := os.Getenv("REVIEW_BINARY")
	if binary == "" {
		t.Fatal("REVIEW_BINARY required")
	}
	var mu sync.Mutex
	statuses := []string{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Status string `json:"status"`
			Host   string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		statuses = append(statuses, p.Status)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	configPath, base, once := appFixture(t, receiver.URL)
	app := startApp(t, binary, configPath, filepath.Join(filepath.Dir(configPath), "before-stop.log"))
	id := acceptSlowEvent(t, base, once)
	waitFor(t, 13*time.Second, "initial heartbeat", func() bool { mu.Lock(); defer mu.Unlock(); return len(statuses) > 0 })
	mu.Lock()
	countBefore := len(statuses)
	mu.Unlock()
	started := time.Now()
	if err := app.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-app.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("shutdown exceeded budget")
	}
	elapsed := time.Since(started)
	if elapsed < 34*time.Second || elapsed > 39*time.Second {
		t.Fatalf("unexpected drain duration %s", elapsed)
	}
	mu.Lock()
	countAfter := len(statuses)
	for _, s := range statuses {
		if s != "pass" {
			t.Errorf("unexpected heartbeat %q", s)
		}
	}
	mu.Unlock()
	if countAfter != countBefore {
		t.Errorf("heartbeats continued during shutdown: before=%d after=%d", countBefore, countAfter)
	}
	app2 := startApp(t, binary, configPath, filepath.Join(filepath.Dir(configPath), "after-stop.log"))
	waitFor(t, 8*time.Second, "done after graceful cancel and restart", func() bool { return eventDone(t, base, id) })
	app2.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-app2.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle shutdown timed out")
	}
	t.Logf("drain=%s; heartbeat pass=%d, no further ping while stopping; event %s completed after restart", elapsed.Round(time.Millisecond), countAfter, id)
}
