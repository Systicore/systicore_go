package logging

import (
	"net/http"

	"github.com/rs/zerolog"
)

// probePaths are the liveness, readiness and metrics endpoints that an
// orchestrator health check or a Prometheus scrape polls every few
// seconds: the union of what the Systicore backends serve.
var probePaths = map[string]bool{
	"/health":       true,
	"/health/live":  true,
	"/health/ready": true,
	"/healthz":      true,
	"/ready":        true,
	"/metrics":      true,
}

// IsProbePath reports whether path is one of the polled endpoints above.
// Exported so a service can reuse the same set, e.g. to keep probes out of
// its rate limiter.
func IsProbePath(path string) bool { return probePaths[path] }

// AccessLevel is the level of the one access-log line per request: debug
// for a probe that succeeded, so polling every few seconds does not bury
// real traffic, info for everything else — a failing probe included,
// since that is exactly the line someone will look for. Every adapter
// uses it, so net/http, Fiber and any later one agree.
func AccessLevel(path string, status int) zerolog.Level {
	if status < http.StatusBadRequest && IsProbePath(path) {
		return zerolog.DebugLevel
	}
	return zerolog.InfoLevel
}
