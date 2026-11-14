package handlerutil

import "context"

type probeContextKey struct{}

var probeContextKeyVal = probeContextKey{}

// WithProbeContext marks a context as belonging to a model test probe.
func WithProbeContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, probeContextKeyVal, true)
}

// IsProbeContext reports whether the context belongs to a model test probe.
func IsProbeContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(probeContextKeyVal).(bool)
	return v
}

// ModelProbeOutcome is one model probe verdict in the shape both the probe
// owner (the media handlers) and the routes that report it (the dashboard's
// per-model and per-provider test buttons) render. It lives here so the two
// packages share one type instead of each converting a private copy.
type ModelProbeOutcome struct {
	OK        bool
	LatencyMs int64
	Status    int
	Note      string
	Error     string
}
