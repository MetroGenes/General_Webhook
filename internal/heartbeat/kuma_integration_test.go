//go:build kuma_integration

package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

type kumaFixture struct {
	Version           string `json:"version"`
	MonitorID         int    `json:"monitorId"`
	InactiveMonitorID int    `json:"inactiveMonitorId"`
	IntervalSeconds   int    `json:"intervalSeconds"`
	Token             string `json:"token"`
	InactiveToken     string `json:"inactiveToken"`
}

type kumaBeat struct {
	ID     int64  `json:"id"`
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Time   string `json:"time"`
}

// Kuma's own SQLite database is read through its bundled SQLite driver. This
// verifies the receiver's actual status, not merely our HTTP status or payload.
func kumaHistory(t *testing.T, container string, monitorID int) []kumaBeat {
	t.Helper()
	const query = `
const sqlite = require("@louislam/sqlite3");
const db = new sqlite.Database("/app/data/kuma.db", sqlite.OPEN_READONLY);
db.all("SELECT id, status, msg, time FROM heartbeat WHERE monitor_id = ? ORDER BY id DESC LIMIT 64",
    [Number(process.argv[1])], (error, rows) => {
        if (error) { console.error(error.message); process.exitCode = 1; db.close(); return; }
        db.get("SELECT COUNT(*) AS count FROM notification", (error, notification) => {
            if (error) { console.error(error.message); process.exitCode = 1; }
            else console.log(JSON.stringify({rows: rows.reverse(), notifications: notification.count}));
            db.close();
        });
    });
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "node", "-e", query, strconv.Itoa(monitorID))
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read disposable Kuma history: %v: %s", err, data)
	}
	var result struct {
		Rows          []kumaBeat `json:"rows"`
		Notifications int        `json:"notifications"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode Kuma history: %v", err)
	}
	if result.Notifications != 0 {
		t.Fatal("the disposable integration instance must have no notification services")
	}
	return result.Rows
}

func TestUptimeKumaIntegration(t *testing.T) {
	container, baseURL, fixturePath := os.Getenv("KUMA_TEST_CONTAINER"), os.Getenv("KUMA_TEST_BASE_URL"), os.Getenv("KUMA_TEST_FIXTURE")
	if container == "" || baseURL == "" || fixturePath == "" {
		t.Skip("run bash scripts/test-kuma.sh to provision the disposable Kuma fixture")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Scheme != "http" || parsedURL.Hostname() != "127.0.0.1" ||
		!strings.HasPrefix(container, "general-webhook-kuma-") {
		t.Fatal("integration tests require the dedicated loopback-only Kuma fixture")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture kumaFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.MonitorID <= 0 || fixture.InactiveMonitorID <= 0 || fixture.Token == "" || fixture.InactiveToken == "" || fixture.IntervalSeconds != 5 {
		t.Fatal("incomplete or unexpected Kuma fixture")
	}
	appLogs := captureHeartbeatLogs(t)
	st := newTestStore(t)
	var ready atomic.Bool
	ready.Store(true)
	// Shorten only the sender's test cadence; production configuration still
	// validates its documented minimum interval.
	m := New(testConfig(250*time.Millisecond, 2*time.Second, config.HeartbeatTarget{
		Name: "kuma-active", URL: baseURL + "/api/push/" + fixture.Token + "?status=up&msg=OK&ping=", Mode: "kuma",
	}), st, ready.Load, nil)
	defer m.Stop()

	for _, step := range []struct {
		ready bool
		want  int
		msg   string
	}{
		{true, 1, "OK"},
		{false, 0, "not ready"},
		{true, 1, "OK"},
	} {
		ready.Store(step.ready)
		m.ping(context.Background())
		beats := kumaHistory(t, container, fixture.MonitorID)
		if len(beats) == 0 {
			t.Fatal("Kuma did not persist the pushed heartbeat")
		}
		last := beats[len(beats)-1]
		if last.Status != step.want || last.Msg != step.msg {
			t.Fatalf("Kuma interpreted push incorrectly: last=%+v want status=%d msg=%q", last, step.want, step.msg)
		}
		if m.MetricsSnapshot()[0].Failures != 0 {
			t.Fatalf("Kuma did not acknowledge heartbeat: %+v", m.MetricsSnapshot()[0])
		}
	}
	t.Logf("Uptime Kuma %s persisted real up -> down -> up transitions", fixture.Version)

	const unknownToken = "GeneralWebhookUnknownPushToken"
	const querySecret = "IntegrationQuerySecret"
	const headerSecret = "IntegrationHeaderSecret"
	bad := New(testConfig(time.Second, 2*time.Second,
		config.HeartbeatTarget{
			Name: "kuma-unknown", URL: baseURL + "/api/push/" + unknownToken + "?token=" + querySecret, Token: headerSecret, Mode: "kuma",
		},
		config.HeartbeatTarget{
			Name: "kuma-inactive", URL: baseURL + "/api/push/" + fixture.InactiveToken, Mode: "kuma",
		},
	), st, nil, nil)
	bad.ping(context.Background())
	for _, metrics := range bad.MetricsSnapshot() {
		if metrics.Attempts != 1 || metrics.Failures != 1 || metrics.LogFailures != 0 ||
			metrics.LastSuccess != 0 || metrics.LastHTTPStatus != 404 {
			t.Fatalf("unknown/inactive Kuma token counted as accepted: %+v", metrics)
		}
	}
	if bad.LastSuccess() != 0 {
		t.Fatal("failed Kuma pushes refreshed global LastSuccess")
	}
	entries, err := st.ListHeartbeatErrors(context.Background(), 100)
	if err != nil || len(entries) != 2 {
		t.Fatalf("real rejected Kuma pushes not recorded in SQLite: entries=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.Origin != baseURL || entry.HTTPStatus != 404 || entry.Mode != "kuma" || entry.Error == "" {
			t.Errorf("incomplete Kuma rejection audit entry: %+v", entry)
		}
	}
	persisted, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.Token, fixture.InactiveToken, unknownToken, querySecret, headerSecret} {
		if strings.Contains(appLogs.String(), secret) || bytes.Contains(persisted, []byte(secret)) {
			t.Fatal("a push credential leaked to application or SQLite logs")
		}
	}
	t.Log("Unknown and inactive Kuma push tokens returned 404; both failures were recorded in SQLite without credentials")

	// Observe actual periodic pushes, then stop them. Stop must not manufacture
	// an immediate down event; Kuma must later detect missing pushes itself.
	attemptsBefore := m.MetricsSnapshot()[0].Attempts
	m.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for m.MetricsSnapshot()[0].Attempts < attemptsBefore+2 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if m.MetricsSnapshot()[0].Attempts < attemptsBefore+2 {
		t.Fatal("periodic heartbeat did not run")
	}
	m.Stop()
	afterStop := m.MetricsSnapshot()[0]
	beats := kumaHistory(t, container, fixture.MonitorID)
	last := beats[len(beats)-1]
	if last.Status != 1 {
		t.Fatalf("graceful stop manufactured an unhealthy push: %+v", last)
	}
	time.Sleep(750 * time.Millisecond)
	afterGrace := kumaHistory(t, container, fixture.MonitorID)
	if len(afterGrace) == 0 || afterGrace[len(afterGrace)-1].ID != last.ID {
		t.Fatal("heartbeat continued or emitted down immediately after Stop")
	}
	t.Log("Monitor.Stop awaited shutdown; no further push or immediate down was received")

	deadline = time.Now().Add(time.Duration(fixture.IntervalSeconds+8) * time.Second)
	for time.Now().Before(deadline) {
		beats = kumaHistory(t, container, fixture.MonitorID)
		current := beats[len(beats)-1]
		if current.ID > last.ID && current.Status == 0 && strings.Contains(current.Msg, "No heartbeat in the time window") {
			if m.MetricsSnapshot()[0].Attempts != afterStop.Attempts {
				t.Fatal("stopped monitor resumed sending")
			}
			t.Log("Kuma independently reported down after its missing-heartbeat deadline")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Kuma did not detect the stopped monitor within its missing-heartbeat deadline")
}
