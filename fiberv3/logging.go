// Package fiberv3 adapts systicore_go packages to Fiber v3. Fiber is
// fasthttp-based, so the net/http middlewares cannot be reused; each
// adapter is a thin fiber.Handler over the same core.
package fiberv3

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Systicore/systicore_go/logging"
)

// Logging is logging.Middleware for Fiber v3: a request-scoped logger in
// c.Context() — the context.Context Fiber v3 hands to handlers — so every
// layer below uses zerolog.Ctx(c.Context()) exactly as under net/http or
// gRPC, plus one access-log line per request.
//
// The request id is the incoming X-Request-ID header, else whatever a
// preceding requestid middleware put on the response, else minted. Register
// after Fiber's requestid/recover middlewares if you use them.
func Logging(root zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Get(logging.RequestIDKey)
		if id == "" {
			id = c.GetRespHeader(logging.RequestIDKey)
		}
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(logging.RequestIDKey, id)

		l := root.With().
			Str("request_id", id).
			Str("method", c.Method()).
			Str("path", c.Path()).
			Str("ip", c.IP()).
			Logger()
		c.SetContext(l.WithContext(logging.WithRequestID(c.Context(), id)))

		start := time.Now()
		err := c.Next()

		l.Info().
			Int("status", c.Response().StatusCode()).
			Int("bytes", len(c.Response().Body())).
			Dur("latency", time.Since(start)).
			Msg("request")
		return err
	}
}
