package report

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// Capture enqueues a report and returns immediately. The sender goroutine
// delivers it; a full queue, a closed client or an open circuit drops the
// report and counts the drop (logged at most once a minute) rather than
// blocking the caller — an error reporter must never become the outage.
// Capture also marks ctx's scope as captured (see WithScope and
// Captured). A disabled or nil client ignores the call.
func (c *Client) Capture(ctx context.Context, report Report) {
	markCaptured(ctx)
	c.enqueue(ctx, report)
}

// enqueue is Capture without marking the scope: the log bridges use it,
// since a log line must not silence the explicit capture that may follow.
func (c *Client) enqueue(ctx context.Context, report Report) {
	if !c.Enabled() {
		return
	}
	report = detach(c.prepare(ctx, report))
	c.closeMutex.RLock()
	defer c.closeMutex.RUnlock()
	if c.closed {
		c.dropped.hit(c, "client closed")
		return
	}
	select {
	case c.queue <- report:
	default:
		c.dropped.hit(c, "queue full")
	}
}

// detach copies every string of a report that is about to outlive the
// caller. Zero-copy frameworks (Fiber / fasthttp) hand out strings that
// alias request buffers reused by the next request, so a route, request
// id or error text taken from them would change under the sender.
func detach(report Report) Report {
	report.Type = strings.Clone(report.Type)
	report.Code = strings.Clone(report.Code)
	report.Message = strings.Clone(report.Message)
	report.Trace = strings.Clone(report.Trace)
	report.Action = strings.Clone(report.Action)
	report.Fingerprint = strings.Clone(report.Fingerprint)
	report.User.ID = strings.Clone(report.User.ID)
	report.User.Issuer = strings.Clone(report.User.Issuer)
	report.Context.Route = strings.Clone(report.Context.Route)
	report.Context.RequestID = strings.Clone(report.Context.RequestID)
	if report.Context.Tags != nil {
		tags := make(map[string]string, len(report.Context.Tags))
		for key, value := range report.Context.Tags {
			tags[strings.Clone(key)] = strings.Clone(value)
		}
		report.Context.Tags = tags
	}
	return report
}

// Dropped returns how many reports were dropped since start (queue full,
// client closed, circuit open) — worth a gauge in the service's metrics.
func (c *Client) Dropped() int64 {
	if !c.Enabled() {
		return 0
	}
	return c.dropped.total.Load()
}

// Close stops accepting reports and flushes what is queued, bounded by
// ctx: when ctx ends first, the sender is cancelled and whatever is still
// queued is lost. Close is idempotent and a no-op on a disabled client.
func (c *Client) Close(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	c.closeMutex.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.closeMutex.Unlock()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		c.stopSender()
		return ctx.Err()
	}
}

// run is the single sender: reports go out one at a time, in order.
func (c *Client) run() {
	defer close(c.done)
	defer c.stopSender()
	for report := range c.queue {
		if c.senderContext.Err() != nil {
			c.dropped.hit(c, "client closed before the queue was flushed")
			continue
		}
		c.recordUndelivered(report, c.deliver(c.senderContext, report))
	}
}

// finalDeliveryTimeout bounds deliverBeforeExit: long enough for one
// round trip and a retry, short enough not to hold up a dying process.
const finalDeliveryTimeout = 2 * time.Second

// deliverBeforeExit sends a report synchronously, bounded by
// finalDeliveryTimeout, for the moments right before the process ends
// (zerolog's Fatal exits right after its hooks ran), when a queued report
// would die with the sender goroutine. Failures are recorded like the
// sender's.
func (c *Client) deliverBeforeExit(ctx context.Context, report Report) {
	deliveryContext, cancel := context.WithTimeout(context.Background(), finalDeliveryTimeout)
	defer cancel()
	report = c.prepare(ctx, report)
	c.recordUndelivered(report, c.deliver(deliveryContext, report))
}

// recordUndelivered accounts for a delivery that failed (err != nil). A
// rejected key only counts a drop, since the circuit breaker logged it
// once already. Any other failure is logged with the report's identity,
// so a payload bug can be found. The report itself is dropped: the backend
// groups, so the next occurrence still counts.
func (c *Client) recordUndelivered(report Report, err error) {
	switch {
	case err == nil:
	case errors.Is(err, ErrCircuitOpen):
		c.dropped.hit(c, "ingest key rejected, sending paused")
	case errors.Is(err, ErrUnauthorized):
		c.dropped.hit(c, "ingest key rejected")
	default:
		c.logger.Warn().Err(err).
			Str("type", report.Type).
			Str("code", report.Code).
			Str("action", report.Action).
			Msg("error report not delivered")
	}
}

// dropCounter counts drops and logs at most once per minute so a flood of
// errors cannot also flood the log.
type dropCounter struct {
	total     atomic.Int64
	lastLog   atomic.Int64 // unix nanos
	sinceLast atomic.Int64
}

func (d *dropCounter) hit(c *Client, reason string) {
	d.total.Add(1)
	d.sinceLast.Add(1)
	now := time.Now().UnixNano()
	last := d.lastLog.Load()
	if now-last < int64(time.Minute) || !d.lastLog.CompareAndSwap(last, now) {
		return
	}
	c.logger.Warn().
		Str("reason", reason).
		Int64("dropped_since_last_log", d.sinceLast.Swap(0)).
		Int64("dropped_total", d.total.Load()).
		Msg("error reports dropped")
}
