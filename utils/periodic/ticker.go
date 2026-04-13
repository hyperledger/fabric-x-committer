// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0

package periodic

import (
	"context"
	"time"
)

// Ticker returns a channel that yields nil values at the specified interval
// until the context is cancelled. The channel is closed when ctx is done.
// Usage:
//
//	for range Ticker(ctx, interval) {
//	    // do periodic work
//	}
//	return ctx.Err()
func Ticker(ctx context.Context, interval time.Duration) <-chan any {
	ch := make(chan any)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case ch <- nil:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}
