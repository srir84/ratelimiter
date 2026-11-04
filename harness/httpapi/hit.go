package httpapi

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"time"

	"github.com/srir84/ratelimiter/harness/config"
	"github.com/srir84/ratelimiter/limiter"
)

// The /hit headers (HTTP-2, HTTP-3).
const (
	headerUserID    = "X-User-ID"
	headerRequestID = "X-Request-ID"
	maxIDLen        = 64
)

// The refusal selection's modulus: refuse_after_acquire_bp counts per 10,000 requests (§B3).
const refuseModulus = config.MaxRefuseBP

// ids are a request's header IDs that passed their checks ("" for one that did not).
type ids struct {
	user, request string
}

// hit serves GET /hit in the DESIGN §B3 order: arrival time, header checks, the in-flight
// slot, the P deadline, acquire (skipped when the limiter is off, HAR-17), the refusal after
// acquire, the database work, then done() or release() and the response. It writes the
// decision, release and request records (§B8).
func (s *Server) hit(w http.ResponseWriter, r *http.Request) {
	arrival := time.Now() // HTTP-5: first, before the header checks, the slot, the limiter and the DB
	rec := requestRecord{Kind: kindRequest, RunID: s.cfg.RunID, Instance: s.cfg.InstanceNum, ArrivalNs: arrival.UnixNano()}
	if id, admitted := s.admit(w, r, &rec); admitted {
		// The slot is freed only after the request record below is on the run-log channel, so
		// WaitIdle and /health in_flight 0 mean every admitted /hit's records are queued.
		defer s.slots.put()
		s.serveHit(w, r, &rec, id)
	}
	rec.TotalNs = time.Since(arrival).Nanoseconds()
	rec.EpochID = s.lim.Health().EpochID // the epoch the instance joined last (plan choice)
	s.log.Enqueue(rec)
}

// admit runs the checks before the limiter: the method, the header IDs (HTTP-2, HTTP-3) and
// the non-blocking in-flight slot (HAR-14). It answers a request that fails one and returns
// false; true means the request holds a slot, which the caller frees.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, rec *requestRecord) (ids, bool) {
	userID, userOK := headerID(r.Header, headerUserID)
	requestID, requestOK := headerID(r.Header, headerRequestID)
	id := ids{user: userID, request: requestID}
	rec.UserID, rec.RequestID = userID, requestID
	if r.Method != http.MethodGet {
		writeError(w, rec, http.StatusMethodNotAllowed, errMethodNotAllowed, id)
		return id, false
	}
	if !userOK || !requestOK {
		writeError(w, rec, http.StatusBadRequest, errInvalidRequest, id) // ERR-5
		return id, false
	}
	if !s.slots.take() {
		writeError(w, rec, http.StatusServiceUnavailable, errOverloaded, id) // ERR-7, no acquire
		return id, false
	}
	return id, true
}

// serveHit answers an admitted /hit (it holds an in-flight slot) and fills rec's outcome
// fields.
func (s *Server) serveHit(w http.ResponseWriter, r *http.Request, rec *requestRecord, id ids) {
	userID, requestID := id.user, id.request

	// P starts just before acquire and bounds the DB work too (HAR-11, HTTP-13). It is never
	// cancelled with the request, so a client disconnect cannot change the outcome.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), time.Now().Add(s.p))
	defer cancel()

	enabled := s.cfg.API.LimiterEnabled
	var dec Decision // stays empty, with no permit, when the limiter is off
	if enabled {
		start := time.Now()
		dec = s.lim.Acquire(ctx, userID)
		rec.LimiterNs = time.Since(start).Nanoseconds()
		rec.Result = dec.Kind.String()
		rec.DecisionNs = dec.DecisionNs
		if status, code, refused := refusal(dec.Kind); refused {
			if dec.Kind == limiter.Limited {
				writeLimited(w, rec, dec, id) // ERR-4
				return
			}
			writeError(w, rec, status, code, id)
			return
		}
		rec.DeductionID = deductionID(dec.Permit)
		s.log.Enqueue(decisionRecord{
			Kind:        kindDecision,
			DeductionID: rec.DeductionID,
			UserID:      userID,
			DecisionNs:  dec.DecisionNs,
			Result:      rec.Result,
		})
	}

	// ERR-15: the application refuses the work after admission. release() is a no-op for a
	// degraded permit, and a baseline run has no permit. SIGTERM is checked again inside the
	// database step, up to just before its first statement (DB.Hit's stop, dbclient.Cancelled).
	if refuseSelected(userID, requestID, s.cfg.API.RefuseAfterAcquireBP) || s.shuttingDown() {
		s.release(dec.Permit)
		writeError(w, rec, http.StatusServiceUnavailable, errAborted, id)
		return
	}

	start := time.Now()
	res := s.db.Hit(ctx, userID, requestID, rec.ArrivalNs, s.stopping)
	rec.DBNs = time.Since(start).Nanoseconds()
	release, status, code := dbOutcome(res.Class)
	if release {
		s.release(dec.Permit)
	} else {
		done(dec.Permit)
	}
	if code != "" {
		writeError(w, rec, status, code, id)
		return
	}
	rec.Existed = res.Existed
	body := successBody{
		UserID:    userID,
		RequestID: requestID,
		ArrivalNs: strconv.FormatInt(res.ArrivalNs, 10), // the row's: an existing row keeps its original time (HTTP-10)
		Existed:   res.Existed,
	}
	if enabled {
		body.Degraded = dec.Kind == limiter.Degraded
		if dec.Kind == limiter.Allowed {
			body.DecisionNs = strconv.FormatInt(dec.DecisionNs, 10)
		}
	}
	writeJSON(w, rec, http.StatusOK, "", body)
}

// headerID returns the header's value and whether it is valid (HTTP-2, HTTP-3): exactly one
// value of 1-64 characters from [A-Za-z0-9_-]. An invalid one comes back as "".
func headerID(h http.Header, name string) (string, bool) {
	vals := h.Values(name)
	if len(vals) != 1 || !validID(vals[0]) {
		return "", false
	}
	return vals[0], true
}

func validID(v string) bool {
	if len(v) < 1 || len(v) > maxIDLen {
		return false
	}
	for i := range len(v) {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// refuseBucket is the request's refusal bucket (§B3): the FNV-1a 64-bit hash of
// user_id ‖ 0x00 ‖ request_id, mod 10,000. It depends only on the IDs, so baseline and limiter
// runs refuse the same requests.
func refuseBucket(userID, requestID string) uint64 {
	h := fnv.New64a()
	// hash.Hash's Write never returns an error.
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(requestID))
	return h.Sum64() % refuseModulus
}

// refuseSelected reports whether the application refuses the request after its acquire:
// its bucket is below refuse_after_acquire_bp (ERR-15).
func refuseSelected(userID, requestID string, bp int) bool {
	if bp <= 0 {
		return false
	}
	return refuseBucket(userID, requestID) < uint64(bp)
}

// retryHints returns 429's Retry-After in whole seconds and retry_after_ms, each rounded up
// from retry_after_ns (HTTP-9).
func retryHints(retryAfterNs int64) (seconds, ms int64) {
	return ceilDiv(retryAfterNs, int64(time.Second)), ceilDiv(retryAfterNs, int64(time.Millisecond))
}

// ceilDiv is n / d rounded up, for d > 0; 0 for n ≤ 0.
func ceilDiv(n, d int64) int64 {
	if n <= 0 {
		return 0
	}
	q := n / d
	if n%d != 0 {
		q++
	}
	return q
}

// successBody is the 200 JSON (HTTP-6): decision_ns only for an allowed acquire (absent for
// degraded, and in baseline runs, which make no acquire).
type successBody struct {
	UserID     string `json:"user_id"`
	RequestID  string `json:"request_id"`
	ArrivalNs  string `json:"arrival_ns"`
	Existed    bool   `json:"existed"`
	Degraded   bool   `json:"degraded"`
	DecisionNs string `json:"decision_ns,omitempty"`
}

// errorBody is the error JSON (HTTP-7): the stable code, a short message and the IDs that
// passed their checks; a 429 adds retry_after_ms and decision_ns (HTTP-6, HTTP-9).
type errorBody struct {
	Error        string `json:"error"`
	Message      string `json:"message"`
	UserID       string `json:"user_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	RetryAfterMs *int64 `json:"retry_after_ms,omitempty"`
	DecisionNs   string `json:"decision_ns,omitempty"`
}

// writeError writes an error response; rec (nil outside /hit) gets its status and code.
func writeError(w http.ResponseWriter, rec *requestRecord, status int, code string, id ids) {
	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodGet)
	}
	writeJSON(w, rec, status, code, errorBody{Error: code, Message: message(code), UserID: id.user, RequestID: id.request})
}

// writeLimited writes the 429 (ERR-4) with its retry hints and the decision's decision_ns.
func writeLimited(w http.ResponseWriter, rec *requestRecord, dec Decision, id ids) {
	seconds, ms := retryHints(dec.RetryAfterNs)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeJSON(w, rec, http.StatusTooManyRequests, errRateLimited, errorBody{
		Error:        errRateLimited,
		Message:      message(errRateLimited),
		UserID:       id.user,
		RequestID:    id.request,
		RetryAfterMs: &ms,
		DecisionNs:   strconv.FormatInt(dec.DecisionNs, 10),
	})
}

// writeJSON writes body as the JSON response (Content-Type application/json; plan choice).
// rec, when not nil, records the status and code.
func writeJSON(w http.ResponseWriter, rec *requestRecord, status int, code string, body any) {
	if rec != nil {
		rec.HTTPStatus, rec.Error = status, code
	}
	data, err := json.Marshal(body)
	if err != nil { // the bodies are fixed structs of strings, numbers and bools
		status = http.StatusInternalServerError
		data = []byte(`{"error":"internal_error","message":"encoding the response failed"}`)
		if rec != nil {
			rec.HTTPStatus, rec.Error = status, errInternal
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The outcome is decided and recorded; a client that went away cannot change it.
	_, _ = w.Write(append(data, '\n'))
}
