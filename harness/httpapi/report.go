package httpapi

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"ratelimiter/limiter"
)

// healthBody is /health (DESIGN §B8; plan H1 step 9): the library's Health JSON plus the
// API's own counters. listener_queue_len exists only in test builds.
type healthBody struct {
	limiter.Health
	LogDrops         int64  `json:"log_drops"`
	InFlight         int    `json:"in_flight"`
	NofileLimit      uint64 `json:"nofile_limit"`
	LogQueueLen      int    `json:"log_queue_len"`
	ListenerQueueLen *int   `json:"listener_queue_len,omitempty"`
}

// health serves /health, always 200 (plan choice: readiness is the JSON status). It calls
// neither Acquire nor the database (HAR-16).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, nil, http.StatusMethodNotAllowed, errMethodNotAllowed, requestIDs(r))
		return
	}
	body := healthBody{
		Health:      s.lim.Health(),
		LogDrops:    s.log.Drops(),
		InFlight:    s.slots.inUse(),
		NofileLimit: s.cfg.NofileLimit,
		LogQueueLen: s.log.Len(),
	}
	if s.cfg.ListenerQueueLen != nil {
		n := s.cfg.ListenerQueueLen()
		body.ListenerQueueLen = &n
	}
	writeJSON(w, nil, http.StatusOK, "", body)
}

// metrics serves /metrics: the limiter's MetricsSnapshot as Prometheus text (DESIGN §A5.11,
// §B8). It calls neither Acquire nor the database (HAR-16).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, nil, http.StatusMethodNotAllowed, errMethodNotAllowed, requestIDs(r))
		return
	}
	var buf bytes.Buffer
	writePrometheus(&buf, s.lim.Metrics())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The snapshot is already taken; a client that went away changes nothing.
	_, _ = w.Write(buf.Bytes())
}

// notFound answers every path without a route: 404 not_found (ERR-6).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, nil, http.StatusNotFound, errNotFound, requestIDs(r))
}

// requestIDs returns the request's header IDs that pass their checks, for error bodies.
func requestIDs(r *http.Request) ids {
	user, _ := headerID(r.Header, headerUserID)
	request, _ := headerID(r.Header, headerRequestID)
	return ids{user: user, request: request}
}

// writePrometheus renders snap in the Prometheus text format: a TYPE line per family, then
// one line per series; a histogram has its cumulative buckets, the +Inf bucket, _sum and
// _count. Label values come from the snapshot's fixed label tables.
func writePrometheus(buf *bytes.Buffer, snap limiter.MetricsSnapshot) {
	for _, f := range snap.Families {
		fmt.Fprintf(buf, "# TYPE %s %s\n", f.Name, f.Type)
		for _, s := range f.Series {
			if f.Type != "histogram" {
				fmt.Fprintf(buf, "%s%s %s\n", f.Name, labels(f.LabelName, s.LabelValue, ""), promFloat(s.Value))
				continue
			}
			for _, b := range s.Buckets {
				fmt.Fprintf(buf, "%s_bucket%s %d\n", f.Name, labels(f.LabelName, s.LabelValue, promFloat(b.UpperBound)), b.Count)
			}
			fmt.Fprintf(buf, "%s_bucket%s %d\n", f.Name, labels(f.LabelName, s.LabelValue, "+Inf"), s.Count)
			fmt.Fprintf(buf, "%s_sum%s %s\n", f.Name, labels(f.LabelName, s.LabelValue, ""), promFloat(s.Sum))
			fmt.Fprintf(buf, "%s_count%s %d\n", f.Name, labels(f.LabelName, s.LabelValue, ""), s.Count)
		}
	}
}

// labels renders a series' label set: the family's label (when it has one) and a histogram
// bucket's le (when le is not ""); "" for neither.
func labels(name, value, le string) string {
	var parts []string
	if name != "" {
		parts = append(parts, name+`="`+escapeLabel(value)+`"`)
	}
	if le != "" {
		parts = append(parts, `le="`+le+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel escapes a label value for the text format: backslash, double quote, newline.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// promFloat formats a sample value: the shortest exact decimal, or NaN, +Inf, -Inf.
func promFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
