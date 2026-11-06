package telegramstore

import (
	"context"
	"errors"
	"time"
)

// seekFetchResult carries one chunk response. The result channel is buffered
// so a cancelled losing request can exit without waiting for the caller.
type seekFetchResult struct {
	payload   []byte
	accountID int64
	elapsed   time.Duration
	cached    bool
	err       error
}

// raceSeekFetch starts a second request only if the first is slow or fails.
// Each request has the same range, so returning the first success preserves
// stream ordering. A failed hedge must never mask a still-running primary.
func raceSeekFetch(ctx context.Context, delay time.Duration, primary, alternate func(context.Context) seekFetchResult) seekFetchResult {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan seekFetchResult, 2)
	start := func(fetch func(context.Context) seekFetchResult) {
		go func() { results <- fetch(runCtx) }()
	}
	start(primary)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	started, completed := 1, 0
	var failures []error
	for {
		select {
		case <-ctx.Done():
			return seekFetchResult{err: ctx.Err()}
		case <-timer.C:
			if started == 1 {
				start(alternate)
				started++
			}
		case result := <-results:
			completed++
			if result.err == nil {
				return result
			}
			failures = append(failures, result.err)
			if started == 1 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				start(alternate)
				started++
			}
			if completed == started {
				return seekFetchResult{err: errors.Join(failures...)}
			}
		}
	}
}
