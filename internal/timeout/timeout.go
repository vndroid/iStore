// Package timeout carries the deadline check that imgproxy's processing
// pipeline performs between stages.
//
// imgproxy puts CheckTimeout in its `server` package, which also owns the HTTP
// server, request IDs and the monitoring hooks — importing it for one function
// would pull OpenTelemetry, Datadog, New Relic and Prometheus into the graph.
// This is that one function.
package timeout

import (
	"context"
	"errors"
)

// ErrTimeout reports that the request deadline passed mid-processing.
var ErrTimeout = errors.New("processing timed out")

// Check returns ErrTimeout if ctx is done, nil otherwise. The pipeline calls it
// between stages so a slow image gives up instead of holding a worker.
func Check(ctx context.Context) error {
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return ctx.Err()
	default:
		return nil
	}
}
