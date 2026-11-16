package proxy

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// FallbackTransport intercepts outbound HTTP requests and automatically retries
// via a direct connection (Proxy: nil) if the environment proxy or local sandbox proxy
// refuses the connection (CONNECT tunnel failed, 403 Forbidden, blocked-by-allowlist).
//
// Built lazily: constructing a safe transport at package-init time would fire
// http.ProxyFromEnvironment's once-cache before main() ran viper's .env sync,
// freezing an empty proxy view for the whole process.
var (
	defaultDirectTransportOnce sync.Once
	defaultDirectTransport     http.RoundTripper
)

func defaultDirect() http.RoundTripper {
	defaultDirectTransportOnce.Do(func() {
		defaultDirectTransport = NewDirectSafeTransport()
	})
	return defaultDirectTransport
}

type FallbackTransport struct {
	Base   http.RoundTripper
	Direct http.RoundTripper
}

// RoundTrip executes the HTTP request through Base, falling back to Direct if Base suffers a proxy refusal.
func (t *FallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	direct := t.Direct
	if direct == nil {
		direct = defaultDirect()
	}

	// Buffer body if present and not replayable
	var bodyBytes []byte
	if req.Body != nil && req.GetBody == nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
	}

	resp, err := base.RoundTrip(req)
	if isProxyFailure(err, resp, transportConsultedAProxy(base, req)) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		directReq := req.Clone(req.Context())
		if req.GetBody != nil {
			directReq.Body, _ = req.GetBody()
		}
		return direct.RoundTrip(directReq)
	}
	return resp, err
}

// transportConsultedAProxy reports whether a proxy was in play for this request.
//
// It only gates the ERROR path (a proxied attempt that threw may be retried
// direct, an unproxied one may not). A response is never replayed on this
// question — see isProxyFailure.
//
// A transport that does not expose a Proxy func is reported as proxied: pool-backed
// and tls-client transports route through whatever the operator assigned, and their
// failures are what this wrapper exists to recover from.
func transportConsultedAProxy(base http.RoundTripper, req *http.Request) bool {
	// *http.Transport carries Proxy as a func FIELD, not a method, so it has to be
	// read directly: a nil field means this request never had a proxy to fail.
	if transport, ok := base.(*http.Transport); ok {
		if transport.Proxy == nil {
			return false
		}
		proxyURL, err := transport.Proxy(req)
		return err == nil && proxyURL != nil
	}
	// Anything else (pool-backed, tls-client, an opaque wrapper) may be routing
	// through a proxy it does not describe; treat it as proxied so its failures
	// still get the direct leg.
	return true
}

// NewFallbackTransport wraps a transport with automatic direct fallback on proxy refusal.
func NewFallbackTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &FallbackTransport{
		Base:   base,
		Direct: defaultDirect(),
	}
}
