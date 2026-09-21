package report

import (
	"context"
	"sync/atomic"
	"time"
)

// Capture enqueues a report and returns immediately. The sender goroutine
// submits it with retries; a full queue drops the report and logs the
// drop count (rate-limited) rather than blocking the caller — an error
// reporter must never become the outage.
func (c *Client) Capture(ctx context.Context, r Report) {
	select {
	case <-c.closing:
		c.dropped.hit(c, "client closed")
		return
	default:
	}
	select {
	case c.queue <- queued{report: r}:
	default:
		c.dropped.hit(c, "queue full")
	}
}

// Dropped returns how many reports were dropped since start (queue full
// or closed) — worth a gauge in the service's metrics.
func (c *Client) Dropped() int64 { return c.dropped.n.Load() }

// Close stops accepting reports and flushes what is queued, bounded by
// ctx. Reports still queued when ctx expires are dropped.
func (c *Client) Close(ctx context.Context) error {
	select {
	case <-c.closing:
		return nil
	default:
		close(c.closing)
	}
	close(c.queue)
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the single sender: reports go out one at a time, in order, each
// with the Submit retry policy. A permanent rejection is logged with the
// report's identity so the payload bug is findable; a transient failure
// that exhausts the attempts is logged and the report is dropped — the
// backend dedups, so the next occurrence will carry the count anyway.
func (c *Client) run() {
	defer close(c.done)
	for q := range c.queue {
		ctx, cancel := context.WithTimeout(context.Background(), c.sendBudget())
		err := c.Submit(ctx, q.report)
		cancel()
		if err != nil {
			c.log.Warn().Err(err).
				Str("code", q.report.Code).
				Str("action", q.report.Action).
				Msg("error report not delivered")
		}
	}
}

// sendBudget bounds one report's total time: every attempt's timeout plus
// the backoffs between them.
func (c *Client) sendBudget() time.Duration {
	perAttempt := c.http.Timeout
	if perAttempt <= 0 {
		perAttempt = 10 * time.Second
	}
	budget := time.Duration(c.opt.Attempts) * perAttempt
	backoff := c.opt.Backoff
	for i := 1; i < c.opt.Attempts; i++ {
		budget += backoff
		backoff *= 2
	}
	return budget
}

// dropCounter counts drops and logs at most once per minute so a flood of
// errors cannot also flood the log.
type dropCounter struct {
	n        atomic.Int64
	lastLog  atomic.Int64 // unix nanos
	sinceLog atomic.Int64
}

func (d *dropCounter) hit(c *Client, reason string) {
	d.n.Add(1)
	d.sinceLog.Add(1)
	now := time.Now().UnixNano()
	last := d.lastLog.Load()
	if now-last < int64(time.Minute) || !d.lastLog.CompareAndSwap(last, now) {
		return
	}
	c.log.Warn().
		Str("reason", reason).
		Int64("dropped_since_last_log", d.sinceLog.Swap(0)).
		Int64("dropped_total", d.n.Load()).
		Msg("error reports dropped")
}
