package httpapi

import (
	"time"

	"github.com/srir84/ratelimiter/limiter"
)

// The api-N.jsonl record kinds and lifecycle events (DESIGN §B8).
const (
	kindDecision  = "decision"
	kindRequest   = "request"
	kindRelease   = "release"
	kindRefund    = "refund"
	kindLifecycle = "lifecycle"

	eventStart = "start"
	eventReady = "ready"
	eventStop  = "stop"
)

// decisionRecord: Acquire returned allowed or degraded; written before the DB work. A
// degraded decision has no deduction_id and decision_ns 0.
type decisionRecord struct {
	Kind        string `json:"kind"`
	DeductionID string `json:"deduction_id"`
	UserID      string `json:"user_id"`
	DecisionNs  int64  `json:"decision_ns"`
	Result      string `json:"result"`
}

// requestRecord: one completed /hit call. IDs that failed their checks are ""; result is ""
// when no acquire was made (refused before it, or a baseline run); deduction_id is "" without
// an allowed permit; decision_ns is the acquire's (allowed, limited; else 0); arrival_ns is
// this call's own arrival (HTTP-5); the durations are monotonic ns (0 for a step not taken),
// total_ns from arrival until the response was written.
type requestRecord struct {
	Kind        string `json:"kind"`
	RunID       string `json:"run_id"`
	EpochID     string `json:"epoch_id"`
	Instance    int    `json:"instance"`
	UserID      string `json:"user_id"`
	RequestID   string `json:"request_id"`
	ArrivalNs   int64  `json:"arrival_ns"`
	DecisionNs  int64  `json:"decision_ns"`
	DeductionID string `json:"deduction_id"`
	Result      string `json:"result"`
	Existed     bool   `json:"existed"`
	HTTPStatus  int    `json:"http_status"`
	Error       string `json:"error"`
	LimiterNs   int64  `json:"limiter_ns"`
	DBNs        int64  `json:"db_ns"`
	TotalNs     int64  `json:"total_ns"`
}

// releaseRecord: permit.Release() called. release_mono_ns is ns since the process started,
// on the monotonic clock (plan choice).
type releaseRecord struct {
	Kind          string `json:"kind"`
	DeductionID   string `json:"deduction_id"`
	ReleaseMonoNs int64  `json:"release_mono_ns"`
}

// refundRecord: one refund listener event (API-1d), as the limiter gave it. Its monotonic
// times are the limiter's (ns since it was built).
type refundRecord struct {
	Kind             string `json:"kind"`
	DeductionID      string `json:"deduction_id"`
	Result           string `json:"result"`
	Reason           string `json:"reason"`
	Attempts         int    `json:"attempts"`
	CreditDecisionNs int64  `json:"credit_decision_ns"`
	ReleaseMonoNs    int64  `json:"release_mono_ns"`
	FinalMonoNs      int64  `json:"final_mono_ns"`
}

// startRecord: the limits this process runs with (HAR-14, HAR-15): the in-flight cap, the
// queue length (0: there is no queue), P, the dependency deadline, the drain timeout and the
// effective soft RLIMIT_NOFILE.
type startRecord struct {
	Kind                 string `json:"kind"`
	Event                string `json:"event"`
	MaxInFlight          int    `json:"max_in_flight"`
	QueueLen             int    `json:"queue_len"`
	MaxReleaseDelayNs    int64  `json:"max_release_delay_ns"`
	DependencyDeadlineNs int64  `json:"dependency_deadline_ns"`
	DrainTimeoutNs       int64  `json:"drain_timeout_ns"`
	NofileLimit          uint64 `json:"nofile_limit"`
}

// readyRecord: the listener is bound and the server is about to serve (plan choice; the
// limiter's own readiness is /health's status).
type readyRecord struct {
	Kind  string `json:"kind"`
	Event string `json:"event"`
}

// stopRecord: the drop counts once the limiter is closed and the run log drained (HAR-13).
type stopRecord struct {
	Kind          string `json:"kind"`
	Event         string `json:"event"`
	ListenerDrops uint64 `json:"listener_drops"`
	LogDrops      int64  `json:"log_drops"`
}

// RefundListener returns the limiter's refund listener (limiter.WithRefundListener): it hands
// each event to log as a refund record and returns at once, since Enqueue never blocks, and
// it never calls the limiter (HOST-8, HAR-13).
func RefundListener(log RunLog) func(limiter.RefundEvent) {
	return func(ev limiter.RefundEvent) {
		log.Enqueue(refundRecord{
			Kind:             kindRefund,
			DeductionID:      ev.DeductionID,
			Result:           ev.Result.String(),
			Reason:           ev.Reason.String(),
			Attempts:         ev.Attempts,
			CreditDecisionNs: ev.CreditDecisionNs,
			ReleaseMonoNs:    ev.ReleaseMonoNs,
			FinalMonoNs:      ev.FinalMonoNs,
		})
	}
}

// LogStart writes the lifecycle start record.
func (s *Server) LogStart() {
	s.log.Enqueue(startRecord{
		Kind:                 kindLifecycle,
		Event:                eventStart,
		MaxInFlight:          s.cfg.API.MaxInFlight,
		QueueLen:             0,
		MaxReleaseDelayNs:    s.cfg.Instance.MaxReleaseDelayNs,
		DependencyDeadlineNs: s.cfg.Instance.DependencyDeadlineNs,
		DrainTimeoutNs:       s.cfg.Instance.DrainTimeoutNs,
		NofileLimit:          s.cfg.NofileLimit,
	})
}

// LogReady writes the lifecycle ready record.
func (s *Server) LogReady() {
	s.log.Enqueue(readyRecord{Kind: kindLifecycle, Event: eventReady})
}

// logStop writes the lifecycle stop record (Stop).
func (s *Server) logStop() {
	s.log.Enqueue(stopRecord{
		Kind:          kindLifecycle,
		Event:         eventStop,
		ListenerDrops: s.lim.Health().ListenerDrops,
		LogDrops:      s.log.Drops(),
	})
}

// release calls p.Release() for a confirmed failure, writing the release record first; it
// does nothing without a permit (a baseline run). The release time is taken just before the
// call.
func (s *Server) release(p Permit) {
	if p == nil {
		return
	}
	s.log.Enqueue(releaseRecord{
		Kind:          kindRelease,
		DeductionID:   p.DeductionID(),
		ReleaseMonoNs: time.Since(s.cfg.ProcessStart).Nanoseconds(),
	})
	p.Release()
}

// done calls p.Done() for success or an unknown outcome; nothing without a permit.
func done(p Permit) {
	if p != nil {
		p.Done()
	}
}

// deductionID is p's deduction_id, "" without a permit.
func deductionID(p Permit) string {
	if p == nil {
		return ""
	}
	return p.DeductionID()
}
