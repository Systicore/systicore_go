// Package fiberv2 adapts systicore_go packages to Fiber v2 (biharvideke,
// spike). Same shape as fiberv3, with v2's split context API.
package fiberv2

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

// Logging is logging.Middleware for Fiber v2. The request-scoped logger
// goes into c.UserContext() — the context.Context of the request. In v2
// c.Context() is the *fasthttp.RequestCtx; it implements context.Context
// but its Value() only sees fasthttp user values, so zerolog.Ctx cannot
// find the logger there. Pass c.UserContext() down to services and
// repositories, not c.Context().
func Logging(root zerolog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get(logging.RequestIDKey)
		if id == "" {
			id = c.GetRespHeader(logging.RequestIDKey)
		}
		if id == "" {
			id = uuid.NewString()
		}
		// c.Get and c.GetRespHeader alias fasthttp buffers that the next
		// request reuses; the id outlives the handler in the context
		// (goroutines, queued error reports), so it gets its own copy.
		id = strings.Clone(id)
		c.Set(logging.RequestIDKey, id)

		l := root.With().
			Str("request_id", id).
			Str("method", c.Method()).
			Str("path", c.Path()).
			Str("ip", c.IP()).
			Logger()
		c.SetUserContext(l.WithContext(logging.WithRequestID(c.UserContext(), id)))

		start := time.Now()
		err := c.Next()

		status := c.Response().StatusCode()
		l.WithLevel(logging.AccessLevel(c.Path(), status)).
			Int("status", status).
			Int("bytes", len(c.Response().Body())).
			Dur("latency", time.Since(start)).
			Msg("request")
		return err
	}
}
