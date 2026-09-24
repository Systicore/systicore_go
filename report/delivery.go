package report

import (
	"context"
	"fmt"
)

// deliver sends one report under the contract's response policy: 2xx
// done; 3xx (never followed) and 400/404/413/422 dropped as ErrRejected;
// 401/403 dropped and the circuit opened for circuitCooldown; 429 waits
// Retry-After and retries; 5xx and network errors retry with doubling
// backoff. Every attempt counts toward Options.Attempts. ctx bounds the
// whole delivery.
func (c *Client) deliver(ctx context.Context, report Report) error {
	body, err := c.encode(report)
	if err != nil {
		return err
	}
	backoff := c.options.Backoff
	var lastError error
	for attempt := 1; attempt <= c.options.Attempts; attempt++ {
		if c.gate.circuitOpen() {
			return ErrCircuitOpen
		}
		if err := c.gate.waitWhilePaused(ctx); err != nil {
			return err
		}
		response, sendError := c.transport.send(ctx, body)
		switch classify(response, sendError) {
		case outcomeDelivered:
			return nil
		case outcomeRejected:
			return rejectedError(response.status)
		case outcomeUnauthorized:
			c.openCircuit(response.status)
			return fmt.Errorf("%w: %w (HTTP %d)", ErrRejected, ErrUnauthorized, response.status)
		case outcomeRateLimited:
			c.gate.pauseFor(response.retryAfter)
			lastError = fmt.Errorf("%w (HTTP %d, retry after %s)", ErrRateLimited, response.status, response.retryAfter)
		case outcomeRetryable:
			lastError = retryableError(response, sendError)
			if attempt < c.options.Attempts {
				if err := c.clock.Sleep(ctx, backoff); err != nil {
					return err
				}
				backoff *= 2
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastError
}

func (c *Client) openCircuit(status int) {
	if c.gate.openCircuit() {
		c.logger.Warn().
			Int("status", status).
			Dur("paused_for", circuitCooldown).
			Msg("reports backend rejected the ingest key; error reporting paused")
	}
}

// rejectedError explains a permanent rejection. A 3xx means BaseURL
// answers with a redirect, which is never followed so that the key cannot
// reach another host: BaseURL has to name the final address.
func rejectedError(status int) error {
	if isRedirect(status) {
		return fmt.Errorf("%w (HTTP %d: redirect not followed, set BaseURL to the final address)", ErrRejected, status)
	}
	return fmt.Errorf("%w (HTTP %d)", ErrRejected, status)
}

func retryableError(response ingestResponse, sendError error) error {
	if sendError != nil {
		return sendError
	}
	return fmt.Errorf("report: reports backend answered HTTP %d", response.status)
}
