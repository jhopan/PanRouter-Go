package proxy

import (
	"bytes"
	"io"
	"net/http"
	"reflect"
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

// transportConsultedAProxy reports whether this request went through a proxy on
// the base leg. A proxy can only refuse a request that reached it: with no proxy
// configured the direct leg is the same path, so a 403 from the application is the
// application's answer and must not be replayed.
//
// A base transport that does not expose a Proxy func is reported as proxied —
// pool-backed and tls-client transports route through whatever the operator
// assigned, and their refusals are exactly what this transport exists to recover
// from.
func transportConsultedAProxy(base http.RoundTripper, req *http.Request) bool {
	transport, ok := base.(*http.Transport)
	if !ok {
		// A pool-backed or tls-client transport routes through whatever the
		// operator assigned; its refusals are what this wrapper exists to recover
		// from, so it keeps the sniff.
		return true
	}
	if transport.Proxy == nil {
		return false
	}
	// The ambient environment is not an assignment. This repo already reads an
	// env-inherited proxy as "not assigned" when it decides whether a refusal may
	// be answered directly (proxiesViaAssignment), and the same distinction
	// matters here: an application 403 served as text/plain must not be replayed
	// just because HTTP_PROXY happens to be set in the process.
	if reflect.ValueOf(transport.Proxy).Pointer() == reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		return false
	}
	proxyURL, err := transport.Proxy(req)
	return err == nil && proxyURL != nil
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
