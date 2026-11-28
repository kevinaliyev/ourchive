package agentcore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunHubLoopStopsAfterMaxRetriesAndResetsOnConnect(t *testing.T) {
	ctx := context.Background()
	attempts := 0
	err := runHubLoopWithConnect(ctx, "https://example.test", 3, time.Millisecond, 2*time.Millisecond, func(ctx context.Context, onConnected func()) error {
		attempts++
		// Succeed once on attempt 2, then fail 3 consecutive times (attempts 3, 4, 5).
		if attempts == 2 {
			onConnected()
			return errors.New("connection dropped")
		}
		return errors.New("dial failed")
	}, loopHooks{}, nil)
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	// Attempt 1 (fail 1/3), Attempt 2 (connected -> reset, then drop = fail 1/3),
	// Attempt 3 (fail 2/3), Attempt 4 (fail 3/3 -> stop).
	if attempts != 4 {
		t.Fatalf("expected 4 total attempts (1 fail, 1 connect+drop, 2 more fails), got %d", attempts)
	}
}
