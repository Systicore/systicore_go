// Package httpx is the net/http toolkit the Systicore services share on
// chi, or on any router that takes func(http.Handler) http.Handler:
// response helpers, the client-IP, recover, security-header and
// body-limit middleware, and the combinators that keep one of them off
// some paths.
//
// It follows the rules of this module: no web framework, no service
// config, and the domain layer never imports it — handlers and the
// router do.
//
// The answers are the ones the Fiber services gave, so a client that
// worked against a service on Fiber keeps working after its move to chi:
// a framework error is {"error": <status text>}, a route nothing matches
// {"error": "Cannot GET /path"}.
package httpx

import "net/http"

// Middleware is the shape chi's Use and With take.
type Middleware = func(http.Handler) http.Handler
