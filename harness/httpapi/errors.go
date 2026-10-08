package httpapi

import (
	"net/http"

	"ratelimiter/harness/dbclient"
	"ratelimiter/limiter"
)

// The stable error codes of the HARNESS ERR table (HTTP-7), the only source of HTTP outcomes
// (HAR-18).
const (
	errRateLimited           = "rate_limited"                  // ERR-4
	errInvalidRequest        = "invalid_request"               // ERR-5, ERR-12d
	errNotFound              = "not_found"                     // ERR-6
	errMethodNotAllowed      = "method_not_allowed"            // ERR-6
	errOverloaded            = "service_overloaded"            // ERR-7
	errLimiterUnavailable    = "limiter_unavailable"           // ERR-8
	errLimiterOutcomeUnknown = "limiter_outcome_unknown"       // ERR-9
	errLimiterDeadline       = "limiter_deadline_exceeded"     // ERR-9a
	errLimiterCapacity       = "limiter_capacity_exceeded"     // ERR-10
	errLimiterIntegrity      = "limiter_state_integrity_error" // ERR-11
	errLimiterConfig         = "limiter_config_error"          // ERR-12
	errLimiterNotReady       = "limiter_not_ready"             // ERR-12a
	errLimiterClock          = "limiter_clock_error"           // ERR-12b
	errLimiterReceipt        = "limiter_receipt_error"         // ERR-12c
	errInternal              = "internal_error"                // ERR-13
	errDBUnavailable         = "database_unavailable"          // ERR-14
	errAborted               = "service_aborted_after_acquire" // ERR-15
	errAppOutcomeUnknown     = "application_outcome_unknown"   // ERR-16
)

// refusal maps an acquire result to its ERR row (HAR-18, HOST-4): ok is false for allowed and
// degraded, which go on to the database work. limited alone gives 429. A kind outside the
// closed set (a library defect) is treated as outcome_unknown: maybe charged, no work.
func refusal(k limiter.ResultKind) (status int, code string, ok bool) {
	switch k {
	case limiter.Allowed, limiter.Degraded:
		return 0, "", false
	case limiter.Limited:
		return http.StatusTooManyRequests, errRateLimited, true
	case limiter.ValidationError:
		return http.StatusBadRequest, errInvalidRequest, true
	case limiter.ClockError:
		return http.StatusServiceUnavailable, errLimiterClock, true
	case limiter.Busy:
		return http.StatusServiceUnavailable, errOverloaded, true
	case limiter.Cancelled:
		return http.StatusServiceUnavailable, errLimiterDeadline, true
	case limiter.ConfigError:
		return http.StatusServiceUnavailable, errLimiterConfig, true
	case limiter.NotReady:
		return http.StatusServiceUnavailable, errLimiterNotReady, true
	case limiter.StateIntegrityError:
		return http.StatusServiceUnavailable, errLimiterIntegrity, true
	case limiter.CapacityError:
		return http.StatusServiceUnavailable, errLimiterCapacity, true
	case limiter.Unavailable:
		return http.StatusServiceUnavailable, errLimiterUnavailable, true
	case limiter.OutcomeUnknown:
		return http.StatusServiceUnavailable, errLimiterOutcomeUnknown, true
	case limiter.ReceiptError:
		return http.StatusServiceUnavailable, errLimiterReceipt, true
	}
	return http.StatusServiceUnavailable, errLimiterOutcomeUnknown, true
}

// dbOutcome maps a database outcome to the permit's finish and the response (DESIGN §B3 DB
// outcome table): release only for a confirmed failure; status 200 with code "" for
// committed. Work that SIGTERM cancelled before its first statement is the refusal after
// acquire (ERR-15). A class outside the closed set is treated as Unknown, the class that
// never refunds.
func dbOutcome(c dbclient.Class) (release bool, status int, code string) {
	switch c {
	case dbclient.Committed:
		return false, http.StatusOK, "" // ERR-1, ERR-2, ERR-3
	case dbclient.FailedNoRow:
		return true, http.StatusServiceUnavailable, errDBUnavailable // ERR-14
	case dbclient.FailedRowMayRemain:
		return true, http.StatusInternalServerError, errInternal // ERR-13
	case dbclient.Cancelled:
		return true, http.StatusServiceUnavailable, errAborted // ERR-15: nothing was sent
	case dbclient.Unknown:
		return false, http.StatusServiceUnavailable, errAppOutcomeUnknown // ERR-16
	}
	return false, http.StatusServiceUnavailable, errAppOutcomeUnknown
}

// message is the short text that goes with an error code (HTTP-7).
func message(code string) string {
	switch code {
	case errRateLimited:
		return "rate limit reached for this user; retry after the hinted delay"
	case errInvalidRequest:
		return "X-User-ID and X-Request-ID must each be one value of 1-64 characters from [A-Za-z0-9_-]"
	case errNotFound:
		return "no such route"
	case errMethodNotAllowed:
		return "method not allowed; use GET"
	case errOverloaded:
		return "service overloaded; request refused before any work"
	case errLimiterUnavailable:
		return "rate limiter store unavailable; request refused"
	case errLimiterOutcomeUnknown:
		return "rate limiter outcome unknown; request refused"
	case errLimiterDeadline:
		return "deadline passed before the rate limiter call was sent"
	case errLimiterCapacity:
		return "rate limiter capacity exceeded"
	case errLimiterIntegrity:
		return "rate limiter state integrity error"
	case errLimiterConfig:
		return "rate limiter configuration error"
	case errLimiterNotReady:
		return "rate limiter not ready"
	case errLimiterClock:
		return "rate limiter clock error"
	case errLimiterReceipt:
		return "rate limiter receipt error; the allowance was charged"
	case errInternal:
		return "processing failed after the work was confirmed started"
	case errDBUnavailable:
		return "database unavailable; nothing was stored"
	case errAborted:
		return "service refused the work after admission"
	case errAppOutcomeUnknown:
		return "database outcome unknown; the row may exist"
	}
	return "error"
}
