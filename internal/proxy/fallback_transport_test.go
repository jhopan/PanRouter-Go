package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// A provider's own 403 is not a proxy refusal, however the body is framed.
//
// The transport used to read any 403 whose Content-Type sniffed as text/plain as
// "the proxy refused this", and replayed the SAME request through the direct leg —
// twice, once inside the transport and once in DoRequest. Measured on one call to
// a provider that answers `403 {"code":11140,...}` with no Content-Type: THREE
// upstream requests, identical headers (including the per-turn ids) and identical
// body. For a lane that bans accounts over repeated request patterns, three copies
// of one turn is the pattern.
func TestFallbackTransportDoesNotReplayAProviderRefusal(t *testing.T) {
	// The fake upstream listens on loopback, which the SSRF guard blocks unless the
	// suite opts in exactly as the integration harness does.
	t.Setenv("ALLOW_PRIVATE_NETWORKING", "true")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// No Content-Type: Go sniffs text/plain, which is also what a proxy page
		// looks like. The body is the provider's own JSON answer.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":11140,"msg":"request illegal"}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewFallbackTransport(NewSafeTransport())}
	resp, err := DoRequest(context.Background(), client, http.MethodPost, srv.URL, nil, []byte(`{"model":"x"}`))
	if resp != nil {
		resp.Body.Close()
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1: a provider 403 must not be replayed", got)
	}
	// The refusal still reaches the caller as the upstream's own status.
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue == nil || ue.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 UpstreamError", err)
	}
}

// A refusal the PROXY names is still replayed: that is what this wrapper is for.
func TestFallbackTransportStillReplaysAProxyRefusal(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_NETWORKING", "true")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// The first leg is refused by the proxy, and says so.
			w.Header().Set("X-Proxy-Error", "blocked-by-allowlist")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Access blocked by the sandbox proxy"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewFallbackTransport(NewSafeTransport())}
	resp, err := DoRequest(context.Background(), client, http.MethodPost, srv.URL, nil, []byte(`{"model":"x"}`))
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("the replay should have succeeded: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want the refused leg plus one direct replay", got)
	}
}

// TestIsProxyFailure_MatchesPanRouter pins the two questions PanRouter answers,
// and only those. The third case is the residual one an earlier local version got
// wrong: an assigned proxy plus a plain 403 from an application is NOT a proxy
// refusal, so nothing is replayed.
func TestIsProxyFailure_MatchesPanRouter(t *testing.T) {
	plain := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"text/plain"}}}
	tests := []struct {
		name       string
		err        error
		resp       *http.Response
		proxyInUse bool
		want       bool
	}{
		{name: "a proxied attempt that threw", err: errors.New("socket hang up"), proxyInUse: true, want: true},
		{name: "a direct attempt that threw", err: errors.New("socket hang up"), proxyInUse: false, want: false},
		{name: "an application 403 as text/plain", resp: plain, proxyInUse: true, want: false},
		{
			name:       "a proxy that names itself",
			resp:       &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Proxy-Error": {"blocked-by-allowlist"}}},
			proxyInUse: true, want: true,
		},
		{
			name:       "a provider's 403 with a JSON body",
			resp:       &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}},
			proxyInUse: true, want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isProxyFailure(tt.err, tt.resp, tt.proxyInUse); got != tt.want {
				t.Errorf("isProxyFailure = %v, want %v", got, tt.want)
			}
		})
	}
}

// failingTransport stands in for a proxy leg that died before answering.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connect tunnel failed")
}

// TestFallbackTransportReplaysAProxiedAttemptThatThrew is PanRouter's `catch`
// branch: the attempt never reached the application, so the direct egress gets a
// turn.
func TestFallbackTransportReplaysAProxiedAttemptThatThrew(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_NETWORKING", "true")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: &FallbackTransport{Base: failingTransport{}, Direct: http.DefaultTransport}}
	resp, err := DoRequest(context.Background(), client, http.MethodPost, srv.URL, nil, []byte(`{"model":"x"}`))
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("the direct replay should have served the turn: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want the single direct replay", got)
	}
}

func TestTransportConsultedAProxy(t *testing.T) {
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.invalid"}}

	tests := []struct {
		name string
		base http.RoundTripper
		want bool
	}{
		{name: "no proxy func at all", base: &http.Transport{}, want: false},
		{
			name: "an explicitly assigned proxy",
			base: NewSafeTransportForProxy("http://proxy.example:3128"),
			want: true,
		},
		{
			name: "an ambient proxy still counts for the error path",
			base: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse("http://proxy.example:3128")
			}},
			want: true,
		},
		// A pool-backed transport is not an *http.Transport, so it keeps the sniff.
		{name: "an opaque transport keeps the sniff", base: opaqueTransport{}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transportConsultedAProxy(tt.base, req); got != tt.want {
				t.Errorf("transportConsultedAProxy = %v, want %v", got, tt.want)
			}
		})
	}
}

// opaqueTransport stands in for a pool-backed or tls-client transport.
type opaqueTransport struct{}

func (opaqueTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
