package server

import (
	"context"
	"runtime"
	"time"

	"github.com/danielpina1/athena/internal/metrics"
)

// resourceInterval is how often the server samples its own resource use.
const resourceInterval = 10 * time.Second

// sampleResources sets the process_rss_bytes and goroutines gauges now and every resourceInterval until ctx is done.
func sampleResources(ctx context.Context, reg *metrics.Registry) {
	rss := reg.Gauge("process_rss_bytes")
	goroutines := reg.Gauge("goroutines")
	sample := func() {
		rss.Set(float64(processRSS()))
		goroutines.Set(float64(runtime.NumGoroutine()))
	}
	sample()
	tick := time.NewTicker(resourceInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sample()
		}
	}
}
