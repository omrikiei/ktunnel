package cmd

import "context"

// waitForReady blocks until the deployment reports its rollout status on
// readyChan, or until ctx is cancelled.
//
// The signal handlers in expose and inject cancel ctx, so this is what lets
// Ctrl+C take effect while a rollout is still in progress. Previously both
// commands received on readyChan directly with no escape: the handler ran on
// another goroutine, logged "Got exit signal", tore down the resources and
// then blocked forever because nothing was left to consume `done`, while the
// main goroutine sat on readyChan until the rollout watcher gave up. On a
// deployment that never becomes ready that is a ten-minute hang after the
// user has already asked to quit.
//
// The returned interrupted flag distinguishes "the user asked us to stop"
// from "the rollout finished and failed", because the caller has to respond
// to those differently -- the first already has a teardown in flight.
func waitForReady(ctx context.Context, readyChan <-chan bool) (ready bool, interrupted bool) {
	select {
	case ready = <-readyChan:
		return ready, false
	case <-ctx.Done():
		return false, true
	}
}
