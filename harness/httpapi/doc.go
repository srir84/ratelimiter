// Package httpapi is hitapi's HTTP layer and request handler (DESIGN §B3; HARNESS §2, §3,
// §9.2): GET /hit runs arrival time, header checks, the in-flight cap, the limiter, the
// refusal after acquire and the database work, in the §B3 order, and maps every outcome
// through the HARNESS ERR table; /health and /metrics report without touching the limiter's
// budget or the database (HAR-16). Every response the handler writes carries Cache-Control:
// no-store (HTTP-8); Server.Handler names the few that net/http writes itself. Each /hit
// writes its run-log records (§B8) through the run-log writer, and the refund listener hands
// the limiter's refund events to the same writer (HOST-8).
//
// The limiter, its permits and the database are small interfaces (plan choice), so the
// handler's tests use fakes; FromLimiter adapts the library's limiter. cmd/hitapi builds
// the server and its dependencies and runs the SIGTERM sequence (HAR-15).
package httpapi
