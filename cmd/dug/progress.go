package main

import (
	"log/slog"
	"time"
)

// progressInterval is how often long-running stages report that they are alive.
var progressInterval = 10 * time.Second

// startProgress logs msg with attrs() every progressInterval until the
// returned stop function is called. stop waits for the logger goroutine, so no
// line is written after it returns.
func startProgress(logger *slog.Logger, msg string, attrs func() []any) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	started := time.Now()
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				logger.Info(msg, append(attrs(), "elapsed", time.Since(started).Round(time.Second).String())...)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
