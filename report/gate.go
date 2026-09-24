package report

import (
	"context"
	"sync"
	"time"
)

// clock is the client's view of time, replaced by tests so the 5-minute
// circuit breaker and Retry-After waits run instantly.
type clock interface {
	Now() time.Time
	// Sleep waits for duration or until ctx is done, returning ctx.Err()
	// in the latter case.
	Sleep(ctx context.Context, duration time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// circuitCooldown is how long the client stops sending after the backend
// rejected the key (401/403): a revoked or mistyped key would otherwise
// cost a request and a log line per report.
const circuitCooldown = 5 * time.Minute

// deliveryGate decides whether the sender may talk to the backend right
// now. It holds two independent states: the circuit breaker opened by a
// 401/403 (reports are dropped while it is open) and the pause requested
// by a 429's Retry-After (reports wait while it lasts).
type deliveryGate struct {
	clock clock

	mutex            sync.Mutex
	circuitOpenUntil time.Time
	pausedUntil      time.Time
}

// openCircuit stops delivery for circuitCooldown. It reports whether the
// circuit was closed before, so the caller logs each opening once.
func (g *deliveryGate) openCircuit() (opened bool) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	now := g.clock.Now()
	opened = !now.Before(g.circuitOpenUntil)
	g.circuitOpenUntil = now.Add(circuitCooldown)
	return opened
}

func (g *deliveryGate) circuitOpen() bool {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.clock.Now().Before(g.circuitOpenUntil)
}

// pauseFor extends the rate-limit pause to at least duration from now.
func (g *deliveryGate) pauseFor(duration time.Duration) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	until := g.clock.Now().Add(duration)
	if until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
}

func (g *deliveryGate) pauseRemaining() time.Duration {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.pausedUntil.Sub(g.clock.Now())
}

// waitWhilePaused blocks until the rate-limit pause is over or ctx ends.
func (g *deliveryGate) waitWhilePaused(ctx context.Context) error {
	for {
		remaining := g.pauseRemaining()
		if remaining <= 0 {
			return nil
		}
		if err := g.clock.Sleep(ctx, remaining); err != nil {
			return err
		}
	}
}
