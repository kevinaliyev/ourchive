package agentcore

import (
	"context"
	"log"
	"time"
)

// loopHooks lets the Runner observe a hub's connection loop. Every field is optional.
type loopHooks struct {
	// onAttempt runs before each dial; failures is the current consecutive-failure count.
	onAttempt func(failures int)
	// onConnected runs once the tunnel handshake succeeds.
	onConnected func()
	// onRetry runs after a failure when another attempt is scheduled after wait.
	onRetry func(failures int, err error, wait time.Duration)
}

// runHubLoopWithConnect keeps calling connectFn with exponential backoff until ctx ends or
// maxRetries consecutive attempts fail (maxRetries <= 0 retries forever). A successful
// connection resets the failure count. A receive on wake skips the current backoff wait.
func runHubLoopWithConnect(
	ctx context.Context,
	hubURL string,
	maxRetries int,
	initialBackoff time.Duration,
	maxBackoff time.Duration,
	connectFn func(ctx context.Context, onConnected func()) error,
	hooks loopHooks,
	wake <-chan struct{},
) error {
	if initialBackoff <= 0 {
		initialBackoff = time.Second
	}
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}

	backoff := initialBackoff
	consecutiveFailures := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hooks.onAttempt != nil {
			hooks.onAttempt(consecutiveFailures)
		}
		connected := false
		err := connectFn(ctx, func() {
			connected = true
			consecutiveFailures = 0
			backoff = initialBackoff
			if hooks.onConnected != nil {
				hooks.onConnected()
			}
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			consecutiveFailures = 0
			backoff = initialBackoff
		}
		consecutiveFailures++
		if maxRetries > 0 && consecutiveFailures >= maxRetries {
			log.Printf("[%s] ERROR: failed to connect after %d consecutive attempts (last error: %v); stopping retries for this hub",
				hubURL, consecutiveFailures, err)
			return err
		}
		if maxRetries > 0 {
			log.Printf("[%s] ERROR: tunnel disconnected (attempt %d/%d): %v; reconnecting in %s...",
				hubURL, consecutiveFailures, maxRetries, err, backoff)
		} else {
			log.Printf("[%s] ERROR: tunnel disconnected (%v); reconnecting in %s...",
				hubURL, err, backoff)
		}
		if hooks.onRetry != nil {
			hooks.onRetry(consecutiveFailures, err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
