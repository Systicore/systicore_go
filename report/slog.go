package report

import (
	"context"
	"log/slog"
)

// SlogHandler wraps next — for services on log/slog instead of zerolog —
// so records at slog.LevelError or above also become reports, with the
// Hook's semantics: Code "log.error" (or the Coder code of an error
// attribute), Message the record message (emptyLogMessage when it has
// none), Trace the stack of the logging call site, the request id from
// ctx, and records whose ctx carries a scope marked by Capture /
// CaptureError / MarkReported skipped.
//
// Unlike a zerolog hook, a slog handler sees the attributes: the first
// attribute holding an error sets Type (see ErrorType) and is appended to
// the Message, so slog.ErrorContext(ctx, "sync failed", "err", err)
// groups by the error, not by the constant message. The record's own
// attributes come first, then those bound with Logger.With (at any group
// level), so logger.With("err", err).Error("sync failed") groups by the
// error as well. The same caution as for Hook applies to HTTP error
// handlers: report 5xx through the Fiber adapter and log them with the
// request ctx, which it marks.
//
// Every record still reaches next unchanged. A disabled or nil client
// returns next itself.
//
//	slog.SetDefault(slog.New(reports.SlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
func (c *Client) SlogHandler(next slog.Handler) slog.Handler {
	if !c.Enabled() {
		return next
	}
	return &slogHandler{next: next, client: c}
}

type slogHandler struct {
	next   slog.Handler
	client *Client
	// boundError is the first error bound with Logger.With, the fallback
	// when a record carries no error attribute of its own.
	boundError error
}

// slogFramePrefix peels log/slog's own frames off the top of the stack.
const slogFramePrefix = "log/slog."

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelError || h.next.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError && !Reported(ctx) {
		h.client.enqueue(ctx, h.reportFrom(record, callerStack(2, slogFramePrefix)))
	}
	if !h.next.Enabled(ctx, record.Level) {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *slogHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	boundError := h.boundError
	if boundError == nil {
		boundError = firstErrorOf(attributes)
	}
	return &slogHandler{next: h.next.WithAttrs(attributes), client: h.client, boundError: boundError}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	return &slogHandler{next: h.next.WithGroup(name), client: h.client, boundError: h.boundError}
}

func (h *slogHandler) reportFrom(record slog.Record, trace string) Report {
	report := Report{
		Code:     "log.error",
		Message:  messageOrPlaceholder(record.Message),
		Trace:    trace,
		Severity: SeverityError,
	}
	if err := h.errorOf(record); err != nil {
		report.Type = ErrorType(err)
		if code := ErrorCode(err); code != "" {
			report.Code = code
		}
		report.Message = messageWithError(record.Message, err)
	}
	return report
}

// messageWithError is "message: error", or the error text alone for a
// record logged without a message.
func messageWithError(message string, err error) string {
	if message == "" {
		return err.Error()
	}
	return message + ": " + err.Error()
}

// errorOf is the record's first error attribute, else the first error
// bound with Logger.With.
func (h *slogHandler) errorOf(record slog.Record) error {
	var found error
	record.Attrs(func(attribute slog.Attr) bool {
		found = attributeError(attribute)
		return found == nil
	})
	if found != nil {
		return found
	}
	return h.boundError
}

func firstErrorOf(attributes []slog.Attr) error {
	for _, attribute := range attributes {
		if err := attributeError(attribute); err != nil {
			return err
		}
	}
	return nil
}

// attributeError is the attribute's value when it holds an error.
func attributeError(attribute slog.Attr) error {
	err, _ := attribute.Value.Resolve().Any().(error)
	return err
}
