package cmd

import "context"

// waitForReady blocks until the deployment reports its rollout status on
// readyChan, or until ctx is cancelled.
//
// The signal handlers in expose and inject cancel ctx, so this is what lets
// Ctrl+C take effect while a rollout is still in progress. Both commands used
// to receive on readyChan directly with no escape, so a user who gave up on a
// deployment that was never going to become ready waited out the rollout
// watcher -- ten minutes after asking to quit.
//
// Both outcomes now return into the same deferred teardown, so the interrupted
// flag is not about who cleans up. It is there so the caller can tell a
// shutdown apart from a rollout that finished and failed, and log accordingly:
// reporting "deployment failed to become ready" at Ctrl+C would blame the
// cluster for something the user did.
func waitForReady(ctx context.Context, readyChan <-chan bool) (ready bool, interrupted bool) {
	select {
	case ready = <-readyChan:
		return ready, false
	case <-ctx.Done():
		return false, true
	}
}
