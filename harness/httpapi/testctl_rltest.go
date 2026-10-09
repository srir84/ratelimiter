//go:build rltest

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ratelimiter/harness/fixtures"
)

// Fixtures are the test build's fault fixtures that the /_test routes drive (DESIGN §B6): the
// limiter's store wrapper (FLT-1 b, FLT-3, FLT-4, barriers), the database wrapper (FLT-5,
// FLT-9), the limiter's signer wrapper (FLT-10) and its send observer (the §A5.4 test hook,
// which shows what was sent under FLT-2). cmd/hitapi's rltest construction installs all four;
// a route whose fixture is missing answers 400. The benchmark build has neither the fixtures
// nor the routes (testctl_stub.go).
type Fixtures struct {
	Store  *fixtures.StoreWrapper
	DB     *fixtures.DBWrap
	Signer *fixtures.SignerWrapper
	Sends  *fixtures.SendLog
}

// The /_test routes' own answers (plan choice): JSON {error, message} like every other error.
const (
	errForbidden = "forbidden" // the remote address is not loopback (§B6)
	maxTestBody  = 64 << 10    // a control request's body, in bytes
	// prekillDrain bounds prekill's wait for the run log to empty, after its wait for the
	// in-flight /hit requests (plan choice: the lines go to a local file).
	prekillDrain = 5 * time.Second
	// prekillSlack is how long prekill waits past P for the in-flight /hit requests (plan H1
	// step 11: P + 1 s).
	prekillSlack = time.Second
	// maxControlMs is the largest duration_ms or delay_ms a fault takes (plan choice): the
	// largest whose nanoseconds fit a time.Duration, so a larger value is refused before it
	// could wrap into a short or negative time.
	maxControlMs = math.MaxInt64 / int64(time.Millisecond)
	// defaultDBCount is how many operations a db fault without a count applies to: the next one
	// (plan choice). DESIGN §B6 arms FLT-5 and FLT-9 with {target, mode} alone, and HARNESS §6
	// describes each as one operation's failure (before the insert or after the commit) or one
	// lost commit reply; the database wrapper has no duration, so a fault armed without a count
	// must still end by itself.
	defaultDBCount = 1
	// defaultCorruptField is the receipt field a signer corrupt fault alters when its body names
	// none (plan choice: the first field of the MAC input, §A5.9). DESIGN §B6 arms FLT-10 with
	// {target:"signer", mode} alone; any altered field fails the MAC check that comes first.
	defaultCorruptField = fixtures.FieldEpochID
	// sinceParam is GET /_test/sends's one query parameter: the sequence number after which
	// records are returned (plan choice; absent: 0, every record kept).
	sinceParam = "since"
	// faultsAllow is the Allow header of a 405 from /_test/faults: both methods it serves (RFC
	// 9110 §15.5.6), POST arming a fault and DELETE disarming them all.
	faultsAllow = "DELETE, POST"
)

// testRoutes registers the /_test routes (DESIGN §B6; IMPLEMENTATION_PLAN H1 step 11, H4 step
// 4). Each answers 403 to a remote address that is not loopback, before anything else. They
// take POST, except DELETE /_test/faults, which disarms every fixture, and GET /_test/sends,
// which reads the send observer; another method gets 405 with Allow naming every method the
// route serves: POST, GET, or for /_test/faults "DELETE, POST". The store, database and signer
// wrappers hold at most 64 armed faults each, and the store wrapper at most 64 barrier names
// (plan choice, AGENTS.md §1 Queues): a fault or barrier past a cap is refused, and the route
// answers the refusal with 400 like any other. The send observer keeps its newest 4,096
// records.
func (s *Server) testRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/_test/faults", s.testFaults)
	mux.HandleFunc("/_test/barrier", s.testBarrier)
	mux.HandleFunc("/_test/barrier/release", s.testBarrierRelease)
	mux.HandleFunc("/_test/prekill", s.testPrekill)
	mux.HandleFunc("/_test/sends", s.testSends)
}

// faultRequest is POST /_test/faults (plan choice for the field set: {target, mode, match,
// count, duration_ms, delay_ms, at}, plus field for a signer corrupt fault). target "store"
// arms a store-wrapper fault on the calls match names, for count calls (absent or 0: every
// matching call while it is armed) and for duration_ms (absent or 0: no limit); a delay waits
// delay_ms where at says: before_send, the default when absent (the call returns cancelled,
// sending nothing, when its context ends first), or after_reply (plan H4 step 6). target "db"
// arms a database fault on the next count operations: a count given must be at least 1, and an
// absent count is defaultDBCount, the next operation (DESIGN §B6's FLT-5 and FLT-9 bodies
// {target:"db", mode} carry none); match stays unset (or "any"), and duration_ms, delay_ms and
// at unset. target "signer" arms a signer fault (fail, unusable or corrupt, FLT-10) for count
// calls (absent or 0: every call until DELETE /_test/faults); match stays unset (or "any"), and
// duration_ms, delay_ms and at unset; field names the receipt field a corrupt fault alters
// (absent: defaultCorruptField) and is refused on every other fault. duration_ms and delay_ms
// are at most maxControlMs.
type faultRequest struct {
	Target     string `json:"target"`
	Mode       string `json:"mode"`
	Match      string `json:"match"`
	Count      *int   `json:"count"` // nil when absent (or null)
	DurationMs int64  `json:"duration_ms"`
	DelayMs    int64  `json:"delay_ms"`
	At         string `json:"at"`
	Field      string `json:"field"`
}

// count is the request's count, 0 when absent.
func (r faultRequest) count() int {
	if r.Count == nil {
		return 0
	}
	return *r.Count
}

// barrierRequest is POST /_test/barrier and, with only name, POST /_test/barrier/release.
type barrierRequest struct {
	Name  string `json:"name"`
	At    string `json:"at"`
	Match string `json:"match"`
}

// prekillReply is POST /_test/prekill's answer, 200 or 503 alike: the drop counts, and
// log_queue_len, the run-log drain's own result. It is 0 when the drain completed: every line
// queued before it began (the finished /hit requests' records among them) was written,
// whatever lines were queued meanwhile (refused /hit requests' records, refund events).
// Otherwise it is how many of those lines are not on the file, and the status is 503: those
// whose write failed (each also in log_drops) and, when the drain timed out, those not handed
// to the kernel yet, the line being written included.
type prekillReply struct {
	ListenerDrops uint64 `json:"listener_drops"`
	LogDrops      int64  `json:"log_drops"`
	LogQueueLen   int64  `json:"log_queue_len"`
}

// okReply answers a control request that armed or released a fixture.
type okReply struct {
	OK bool `json:"ok"`
}

// errBadRequest marks a control request refused with 400.
var errBadRequest = errors.New("bad control request")

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errBadRequest}, args...)...)
}

// testFaults serves POST /_test/faults, and DELETE /_test/faults (testDisarm).
func (s *Server) testFaults(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.testDisarm(w, r)
		return
	}
	var req faultRequest
	if !s.checkControl(w, r, http.MethodPost, faultsAllow, &req) {
		return
	}
	if err := s.armFault(req); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, nil, http.StatusOK, "", okReply{OK: true})
}

// armFault checks req and arms its fault. It refuses a time above maxControlMs before
// converting it to a time.Duration.
func (s *Server) armFault(req faultRequest) error {
	if req.count() < 0 || req.DurationMs < 0 || req.DelayMs < 0 {
		return badRequest("count, duration_ms and delay_ms must not be negative")
	}
	if req.DurationMs > maxControlMs || req.DelayMs > maxControlMs {
		return badRequest("duration_ms and delay_ms must be at most %d", maxControlMs)
	}
	switch req.Target {
	case "store":
		return s.armStoreFault(req)
	case "db":
		return s.armDBFault(req)
	case "signer":
		return s.armSignerFault(req)
	default:
		return badRequest("unknown target %q, want \"store\", \"db\" or \"signer\"", req.Target)
	}
}

// armStoreFault arms a store-wrapper fault: refuse (FLT-1 b), drop_reply (FLT-3, FLT-4) or
// delay, before the send or after the reply, on the calls match names.
func (s *Server) armStoreFault(req faultRequest) error {
	if s.cfg.Fixtures.Store == nil {
		return badRequest("target store: this server has no store wrapper")
	}
	if req.Field != "" {
		return badRequest("target store takes no field")
	}
	mode := fixtures.Mode(req.Mode)
	switch mode {
	case fixtures.Refuse, fixtures.DropReply:
		if req.DelayMs != 0 || req.At != "" {
			return badRequest("mode %q takes no delay_ms and no at", req.Mode)
		}
	case fixtures.Delay:
		if req.DelayMs == 0 {
			return badRequest("mode delay needs delay_ms")
		}
		if at := fixtures.At(req.At); req.At != "" && at != fixtures.BeforeSend && at != fixtures.AfterReply {
			return badRequest("at %q, want \"before_send\" or \"after_reply\"", req.At)
		}
	case fixtures.Barrier:
		return badRequest("store mode barrier is armed through /_test/barrier")
	default:
		return badRequest("unknown store mode %q", req.Mode)
	}
	match, err := matchName(req.Match)
	if err != nil {
		return err
	}
	if err := s.cfg.Fixtures.Store.Arm(fixtures.Fault{
		Mode:     mode,
		Match:    match,
		Count:    req.count(),
		Duration: time.Duration(req.DurationMs) * time.Millisecond,
		Delay:    time.Duration(req.DelayMs) * time.Millisecond,
		At:       fixtures.At(req.At),
	}); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// armDBFault arms a database fault on the next count operations (FLT-5, FLT-9), or on the
// next defaultDBCount when the request has no count.
func (s *Server) armDBFault(req faultRequest) error {
	if s.cfg.Fixtures.DB == nil {
		return badRequest("target db: this server has no database wrapper")
	}
	switch mode := fixtures.DBMode(req.Mode); mode {
	case fixtures.FailBefore, fixtures.FailAfterCommit, fixtures.LoseCommitReply:
	default:
		return badRequest("unknown db mode %q", req.Mode)
	}
	if req.Match != "" && req.Match != "any" {
		return badRequest("target db: match %q, want none or \"any\" (a db fault applies to the next operations)", req.Match)
	}
	if req.DurationMs != 0 || req.DelayMs != 0 || req.At != "" || req.Field != "" {
		return badRequest("target db takes no duration_ms, delay_ms, at or field")
	}
	count := defaultDBCount
	if req.Count != nil {
		count = *req.Count
	}
	if count < 1 {
		return badRequest("target db: count %d, want the next N operations, N at least 1", count)
	}
	if err := s.cfg.Fixtures.DB.Arm(fixtures.DBMode(req.Mode), count); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// armSignerFault arms a signer fault (FLT-10, DESIGN §B6): fail (receipt_error after a
// confirmed deduction), unusable (config_error with no charge) or corrupt (the refund's local
// verification fails: receipt_invalid, nothing sent), on the next count calls, or every call
// until DELETE /_test/faults when the count is absent or 0.
func (s *Server) armSignerFault(req faultRequest) error {
	if s.cfg.Fixtures.Signer == nil {
		return badRequest("target signer: this server has no signer wrapper")
	}
	mode := fixtures.SignerMode(req.Mode)
	field := req.Field
	switch mode {
	case fixtures.SignFail, fixtures.SignUnusable:
		if field != "" {
			return badRequest("signer mode %q takes no field", req.Mode)
		}
	case fixtures.SignCorrupt:
		if field == "" {
			field = defaultCorruptField
		}
	default:
		return badRequest("unknown signer mode %q, want \"fail\", \"unusable\" or \"corrupt\"", req.Mode)
	}
	if req.Match != "" && req.Match != "any" {
		return badRequest("target signer: match %q, want none or \"any\" (a signer fault applies to the signer's next calls)", req.Match)
	}
	if req.DurationMs != 0 || req.DelayMs != 0 || req.At != "" {
		return badRequest("target signer takes no duration_ms, delay_ms or at")
	}
	if err := s.cfg.Fixtures.Signer.Arm(fixtures.SignerFault{Mode: mode, Count: req.count(), Field: field}); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// testDisarm serves DELETE /_test/faults (plan choice, IMPLEMENTATION_PLAN H4 step 4): every
// armed store fault, barrier fault, database fault and signer fault is removed, and every call
// blocked at a barrier goes on (StoreWrapper.Disarm). The body is empty or {}.
func (s *Server) testDisarm(w http.ResponseWriter, r *http.Request) {
	if !s.controlRequest(w, r, http.MethodDelete, nil) {
		return
	}
	f := s.cfg.Fixtures
	if f.Store != nil {
		f.Store.Disarm()
	}
	if f.DB != nil {
		f.DB.Disarm()
	}
	if f.Signer != nil {
		f.Signer.Disarm()
	}
	writeJSON(w, nil, http.StatusOK, "", okReply{OK: true})
}

// testSends serves GET /_test/sends?since=<seq> (plan choice, IMPLEMENTATION_PLAN H4 step 4;
// DESIGN §A5.4 test hook): the send observer's records whose seq is above since (absent: 0),
// oldest first, as a JSON array of {seq, cmd, bytes_written}: cmd is the script name for an
// EVALSHA, else the command (§A5.12), and bytes_written above 0 means the command was sent. A
// record the observer's ring has dropped shows as a gap in seq. The query may hold since once
// and nothing else, and the body is empty or {}.
func (s *Server) testSends(w http.ResponseWriter, r *http.Request) {
	if !s.controlRequest(w, r, http.MethodGet, nil) {
		return
	}
	if s.cfg.Fixtures.Sends == nil {
		writeControlError(w, badRequest("this server has no send observer"))
		return
	}
	since, err := sinceOf(r)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, nil, http.StatusOK, "", s.cfg.Fixtures.Sends.Since(since))
}

// sinceOf reads GET /_test/sends's query: since, at most once, a decimal sequence number of at
// least 0 (absent: 0), and no other parameter.
func sinceOf(r *http.Request) (uint64, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, badRequest("the query does not parse: %v", err)
	}
	for k, vs := range q {
		if k != sinceParam || len(vs) != 1 {
			return 0, badRequest("the query takes %s=<seq> once and nothing else", sinceParam)
		}
	}
	v, ok := q[sinceParam]
	if !ok {
		return 0, nil
	}
	since, err := strconv.ParseUint(v[0], 10, 64)
	if err != nil {
		return 0, badRequest("%s=%q: want a sequence number of at least 0", sinceParam, v[0])
	}
	return since, nil
}

// matchName maps a control request's match to the store wrapper's: a script name, or "" for
// every call ("any").
func matchName(match string) (string, error) {
	switch match {
	case "acquire", "refund":
		return match, nil
	case "any":
		return "", nil
	default:
		return "", badRequest("match %q, want \"acquire\", \"refund\" or \"any\"", match)
	}
}

// testBarrier serves POST /_test/barrier {name, at, match}: the store wrapper blocks the
// matching calls before their send or after their reply until the barrier is released (§B6).
// An absent match is "any", every call: DESIGN §B6's body is {name, at}.
func (s *Server) testBarrier(w http.ResponseWriter, r *http.Request) {
	var req barrierRequest
	if !s.controlRequest(w, r, http.MethodPost, &req) {
		return
	}
	if err := s.armBarrier(req); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, nil, http.StatusOK, "", okReply{OK: true})
}

func (s *Server) armBarrier(req barrierRequest) error {
	if s.cfg.Fixtures.Store == nil {
		return badRequest("barriers: this server has no store wrapper")
	}
	if req.Name == "" {
		return badRequest("a barrier needs a name")
	}
	at := fixtures.At(req.At)
	if at != fixtures.BeforeSend && at != fixtures.AfterReply {
		return badRequest("at %q, want \"before_send\" or \"after_reply\"", req.At)
	}
	if req.Match == "" {
		req.Match = "any"
	}
	match, err := matchName(req.Match)
	if err != nil {
		return err
	}
	if err := s.cfg.Fixtures.Store.Arm(fixtures.Fault{Mode: fixtures.Barrier, Match: match, At: at, Name: req.Name}); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// testBarrierRelease serves POST /_test/barrier/release {name}: the barrier's faults are
// disarmed and every call blocked at it goes on (StoreWrapper.DisarmBarrier), so later calls
// no longer meet it and a store fault or barrier armed afterwards on the same calls takes
// effect.
func (s *Server) testBarrierRelease(w http.ResponseWriter, r *http.Request) {
	var req barrierRequest
	if !s.controlRequest(w, r, http.MethodPost, &req) {
		return
	}
	switch {
	case s.cfg.Fixtures.Store == nil:
		writeControlError(w, badRequest("barriers: this server has no store wrapper"))
		return
	case req.Name == "" || req.At != "" || req.Match != "":
		writeControlError(w, badRequest("a release takes the barrier's name only"))
		return
	}
	s.cfg.Fixtures.Store.DisarmBarrier(req.Name)
	writeJSON(w, nil, http.StatusOK, "", okReply{OK: true})
}

// testPrekill serves POST /_test/prekill (H4's kill calls it just before SIGKILL): stop
// accepting /hit, so each new one gets 503 service_overloaded (ERR-7), without refusing the
// admitted ones (no ERR-15: that is SIGTERM's); wait up to P + 1 s until every admitted /hit
// has finished and queued its records; wait up to prekillDrain until the run log has handed
// to the kernel every line queued by then; and answer the drop counts and what that drain
// left (prekillReply). A drain that timed out, or one with a line whose write failed, answers
// 503, so H4's kill records read_ok:false (refund evidence incomplete, HAR-13). The body is
// empty or {}.
func (s *Server) testPrekill(w http.ResponseWriter, r *http.Request) {
	if !s.controlRequest(w, r, http.MethodPost, nil) {
		return
	}
	s.slots.stop()
	// Never cancelled with the request: the caller kills the process next, whatever happens.
	base := context.WithoutCancel(r.Context())
	ictx, cancel := context.WithTimeout(base, s.p+prekillSlack)
	_ = s.WaitIdle(ictx) // on a timeout the drain below covers the records queued by then
	cancel()
	dctx, cancel := context.WithTimeout(base, prekillDrain)
	left, err := s.log.Drain(dctx)
	cancel()
	status := http.StatusOK
	if err != nil {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, nil, status, "", prekillReply{
		ListenerDrops: s.lim.Health().ListenerDrops,
		LogDrops:      s.log.Drops(),
		LogQueueLen:   left,
	})
}

// controlRequest runs the checks every /_test route shares, in order: a loopback remote
// address (else 403), the route's method (else 405 with Allow naming it), and a body that is
// one JSON object whose keys all belong to into, at most maxTestBody bytes (else 400). With
// into nil the body must be empty or {}. It answers a request that fails one and returns false.
func (s *Server) controlRequest(w http.ResponseWriter, r *http.Request, method string, into any) bool {
	return s.checkControl(w, r, method, method, into)
}

// checkControl is controlRequest for a route that serves more methods than the one this
// handler takes: a 405 names allow, every method the route serves (RFC 9110 §15.5.6).
func (s *Server) checkControl(w http.ResponseWriter, r *http.Request, method, allow string, into any) bool {
	if !loopback(r.RemoteAddr) {
		writeJSON(w, nil, http.StatusForbidden, errForbidden, errorBody{
			Error: errForbidden, Message: "the /_test routes answer loopback addresses only",
		})
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", allow)
		writeJSON(w, nil, http.StatusMethodNotAllowed, errMethodNotAllowed, errorBody{
			Error: errMethodNotAllowed, Message: "method not allowed; use " + allow,
		})
		return false
	}
	if err := decodeControl(r.Body, into); err != nil {
		writeControlError(w, err)
		return false
	}
	return true
}

// decodeControl decodes body into into, refusing unknown keys, a second value, and more than
// maxTestBody bytes. With into nil it accepts an empty body or {}.
func decodeControl(body io.Reader, into any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxTestBody+1))
	if err != nil {
		return badRequest("reading the body: %v", err)
	}
	if len(data) > maxTestBody {
		return badRequest("the body is over %d bytes", maxTestBody)
	}
	if into == nil {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		into = &struct{}{}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return badRequest("the body is not one JSON object of the route's fields: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("the body holds more than one JSON value")
	}
	return nil
}

// writeControlError answers a refused control request: 400 invalid_request with the reason.
func writeControlError(w http.ResponseWriter, err error) {
	writeJSON(w, nil, http.StatusBadRequest, errInvalidRequest, errorBody{Error: errInvalidRequest, Message: err.Error()})
}

// loopback reports whether addr (host:port, as http.Request.RemoteAddr) is a loopback IP.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
