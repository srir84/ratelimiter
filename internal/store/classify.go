package store

import "strings"

// classifyErrorReply maps a store error reply to its outcome (DESIGN §A5.4, SPEC API-22).
// It matches the reply's first space-delimited word exactly, so BUSYKEY is not BUSY and a
// lowercase word matches nothing:
//   - NOSCRIPT: the script never ran; reload asks for one SCRIPT LOAD and one resend.
//   - OOM: capacity_error for acquire and refund, whose plain #!lua header makes the store
//     refuse them before they run (§A3.0 row 6, §A4.8 rule 1); outcome_unknown for every
//     other script.
//   - LOADING, BUSY: refused in command dispatch before running (§A3.0 row 16), so
//     unavailable, counted as not sent.
//   - anything else (BUSYKEY, READONLY, ERR, WRONGTYPE, …): outcome_unknown.
func classifyErrorReply(script, text string) (out Outcome, reload bool) {
	word, _, _ := strings.Cut(text, " ")
	switch word {
	case "NOSCRIPT":
		return OutcomeUnknown, true
	case "OOM":
		if script == "acquire" || script == "refund" {
			return CapacityError, false
		}
		return OutcomeUnknown, false
	case "LOADING", "BUSY":
		return Unavailable, false
	}
	return OutcomeUnknown, false
}
