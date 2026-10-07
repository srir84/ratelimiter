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

// NotReadyReply reports whether text, a store error reply, refused its command before it ran:
// LOADING or BUSY by classifyErrorReply's first-word rule (the one rule maps exactly those two
// words to unavailable, for every script). Test wrappers count those refusals with it whatever
// outcome the call ended with: after a NOSCRIPT reload, a resend refused so ends
// outcome_unknown with the reply kept in StoreError.
func NotReadyReply(text string) bool {
	out, _ := classifyErrorReply("", text)
	return out == Unavailable
}
