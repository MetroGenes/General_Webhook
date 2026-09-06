package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) rejectRate(w http.ResponseWriter, layer int) {
	s.rateRejected[layer].Add(1)
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// Prometheus supports \\, \" and \n escapes, unlike Go's general %q encoding.
func prometheusQuote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString("\\\\")
		case '"':
			out.WriteString("\\\"")
		case '\n':
			out.WriteString("\\n")
		default:
			if r < 0x20 || r == 0x7f {
				out.WriteRune('\ufffd')
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

func (s *Server) heartbeatLogs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "limit must be 1..100", http.StatusBadRequest)
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	entries, err := s.st.ListHeartbeatErrors(ctx, limit)
	if err != nil {
		http.Error(w, "store error", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"heartbeat_errors": entries})
}

func (s *Server) heartbeatMetrics(w io.Writer) {
	_, _ = io.WriteString(w, "# HELP general_webhook_heartbeat_target_attempts_total Push attempts per configured target.\n# TYPE general_webhook_heartbeat_target_attempts_total counter\n")
	_, _ = io.WriteString(w, "# HELP general_webhook_heartbeat_target_failures_total Failed pushes per configured target.\n# TYPE general_webhook_heartbeat_target_failures_total counter\n")
	_, _ = io.WriteString(w, "# HELP general_webhook_heartbeat_target_log_failures_total Heartbeat errors that could not be persisted.\n# TYPE general_webhook_heartbeat_target_log_failures_total counter\n")
	_, _ = io.WriteString(w, "# HELP general_webhook_heartbeat_target_last_success_timestamp Last accepted push per target, including down signals.\n# TYPE general_webhook_heartbeat_target_last_success_timestamp gauge\n")
	_, _ = io.WriteString(w, "# HELP general_webhook_heartbeat_target_last_duration_milliseconds Duration of the last target attempt.\n# TYPE general_webhook_heartbeat_target_last_duration_milliseconds gauge\n")
	for _, m := range s.hb.MetricsSnapshot() {
		labels := "target=" + prometheusQuote(m.Name) + ",mode=" + prometheusQuote(m.Mode)
		_, _ = fmt.Fprintf(w, "general_webhook_heartbeat_target_attempts_total{%s} %d\n", labels, m.Attempts)
		_, _ = fmt.Fprintf(w, "general_webhook_heartbeat_target_failures_total{%s} %d\n", labels, m.Failures)
		_, _ = fmt.Fprintf(w, "general_webhook_heartbeat_target_log_failures_total{%s} %d\n", labels, m.LogFailures)
		_, _ = fmt.Fprintf(w, "general_webhook_heartbeat_target_last_success_timestamp{%s} %d\n", labels, m.LastSuccess)
		_, _ = fmt.Fprintf(w, "general_webhook_heartbeat_target_last_duration_milliseconds{%s} %d\n", labels, m.LastDurationMS)
	}
}
