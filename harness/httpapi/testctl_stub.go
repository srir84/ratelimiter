//go:build !rltest

package httpapi

import "net/http"

// Fixtures is empty in the benchmark build, which has no fault fixtures and no /_test routes
// (DESIGN §B6: they are compiled only with -tags rltest; testctl_rltest.go).
type Fixtures struct{}

// testRoutes registers nothing in the benchmark build: every /_test path is a 404.
func (s *Server) testRoutes(*http.ServeMux) {}
