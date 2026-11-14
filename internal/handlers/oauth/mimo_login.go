package oauth

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"panrouter/proxy/internal/handlerutil"
)

// Xiaomi MiMo account-session login: the dashboard opens MiMo Desktop's own
// login surface through this gateway and later reads the captured passToken.
//
// Port of upstream src/app/api/oauth/xiaomi-mimo/login/{start,status}/route.js
// and the cookie-carried session in src/lib/mimoLoginSession.js. The session —
// including the accumulated cookie jar — travels inside the httpOnly
// `9r_mimo_login` cookie itself, because start and status are independent
// requests that share no memory.

const (
	mimoSessionCookie = "9r_mimo_login"
	mimoSessionTTL    = 15 * time.Minute
	// mimoAccountHost is the SSO host whose cookies become the identity. Only
	// account.xiaomi.com cookies count: the mimo-server cluster answers its own
	// cookies and none of them is a login proof.
	mimoAccountHost = "account.xiaomi.com"
	mimoAPIUA       = "miNative PC/Normal Windows_NT/10.0.19045 SDKV/1.0.0 DEVT/PC DEVS/Windows APP/miaccount_desktop APPV/0.1.0"
	mimoSSOUA       = "MiClaw/1.0"
)

// mimoRegionBases are the account clusters MiMo Desktop declares. Unknown or
// absent regions fall back to sgp, the international/open cluster.
var mimoRegionBases = map[string]string{
	"cn":  "https://mimo-server-cn.xiaomimimo.com",
	"sgp": "https://mimo-server-sgp.xiaomimimo.com",
	"ams": "https://mimo-server-ams.xiaomimimo.com",
	"ru":  "https://mimo-server-ru.xiaomimimo.com",
	"in":  "https://mimo-server-in.xiaomimimo.com",
}

const mimoDefaultRegion = "sgp"

// mimoLoginRegions is what POST .../login/start accepts; anything else is
// normalised to cn, the desktop default upstream uses for that route.
var mimoLoginRegions = map[string]bool{"cn": true, "sgp": true, "ams": true, "ru": true, "in": true}

// mimoLoginCookie is one cookie in the session jar.
type mimoLoginCookie struct {
	Name   string
	Value  string
	Domain string
	Path   string
}

// mimoLoginSession is the login walk's state between requests.
type mimoLoginSession struct {
	State        string
	Region       string
	ProxyURL     string
	CreatedAt    time.Time
	UpstreamBase string
	Jar          map[string]mimoLoginCookie
}

// mimoIdentity is what a completed session proved: the passToken plus the
// identity cookies stored beside it.
type mimoIdentity struct {
	PassToken string
	UserID    string
	CUserID   string
}

// mimoHopResult is one redirect leg. The body is drained by the hop itself —
// only the status and Location steer the walk.
type mimoHopResult struct {
	Status   int
	Location string
}

func newMimoLoginSession(region string) *mimoLoginSession {
	if !mimoLoginRegions[region] {
		region = mimoDefaultRegion
	}
	return &mimoLoginSession{
		State:        randomString(32),
		Region:       region,
		CreatedAt:    time.Now(),
		UpstreamBase: mimoRegionBases[region],
		Jar:          map[string]mimoLoginCookie{},
	}
}

func mimoJarKey(c mimoLoginCookie) string {
	return c.Name + "|" + c.Domain + "|" + c.Path
}

func mimoDomainMatch(cookieDomain, host string) bool {
	d := strings.ToLower(strings.TrimPrefix(cookieDomain, "."))
	h := strings.ToLower(host)
	return h == d || strings.HasSuffix(h, "."+d)
}

// mimoDefaultCookiePath is Chromium's rule: the request path up to and
// including the last slash.
func mimoDefaultCookiePath(requestPath string) string {
	if requestPath == "" {
		return "/"
	}
	if i := strings.LastIndex(requestPath, "/"); i >= 0 {
		if p := requestPath[:i+1]; p != "" {
			return p
		}
	}
	return "/"
}

// cookieHeader renders the jar for one upstream host/path. Only cookies that
// match both are sent, so a passToken minted for the account host never leaks
// to the cluster host.
func (s *mimoLoginSession) cookieHeader(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(s.Jar))
	for _, c := range s.Jar {
		if !mimoDomainMatch(c.Domain, u.Hostname()) {
			continue
		}
		if !strings.HasPrefix(u.Path, c.Path) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// byteBudget keeps one cookie value under the browser's 4096-byte limit;
// base64url inflates the JSON by ~1.34, so the payload must stay near 2800.
const mimoCookieJSONBudget = 2800

// mimoKeepOnOverflow is the set of cookies worth the budget when the jar does
// not fit: the identity and device cookies, without which the walk restarts.
var mimoKeepOnOverflow = regexp.MustCompile(`^(passToken|userId|cUserId|serviceToken|.*_serviceToken|.*_ph|.*_slh|deviceId)$`)

type mimoSessionPayload struct {
	State     string     `json:"s"`
	Region    string     `json:"r"`
	CreatedAt int64      `json:"t"`
	ProxyURL  string     `json:"p"`
	Jar       [][]string `json:"j"`
}

// encodeMimoSessionCookie serialises the session into the cookie value.
func encodeMimoSessionCookie(sess *mimoLoginSession) string {
	entries := make([][]string, 0, len(sess.Jar))
	for _, c := range sess.Jar {
		entries = append(entries, []string{c.Name, c.Value, c.Domain, c.Path})
	}
	payload := mimoSessionPayload{
		State:     sess.State,
		Region:    sess.Region,
		CreatedAt: sess.CreatedAt.UnixMilli(),
		ProxyURL:  sess.ProxyURL,
		Jar:       entries,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	if len(encoded) > mimoCookieJSONBudget {
		kept := make([][]string, 0, len(entries))
		for _, entry := range entries {
			if mimoKeepOnOverflow.MatchString(entry[0]) {
				kept = append(kept, entry)
			}
		}
		payload.Jar = kept
		encoded, err = json.Marshal(payload)
		if err != nil {
			return ""
		}
	}
	return "v2." + base64.RawURLEncoding.EncodeToString(encoded)
}

// decodeMimoSessionCookie rebuilds a session from the cookie value, or nil
// when it is missing, malformed, from another era, or past its TTL.
func decodeMimoSessionCookie(value string) *mimoLoginSession {
	const prefix = "v2."
	if !strings.HasPrefix(value, prefix) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return nil
	}
	var payload mimoSessionPayload
	if json.Unmarshal(raw, &payload) != nil || payload.CreatedAt == 0 {
		return nil
	}
	created := time.UnixMilli(payload.CreatedAt)
	if time.Since(created) > mimoSessionTTL {
		return nil
	}
	region := payload.Region
	if !mimoLoginRegions[region] {
		region = mimoDefaultRegion
	}
	sess := &mimoLoginSession{
		State:        payload.State,
		Region:       region,
		ProxyURL:     payload.ProxyURL,
		CreatedAt:    created,
		UpstreamBase: mimoRegionBases[region],
		Jar:          map[string]mimoLoginCookie{},
	}
	for _, entry := range payload.Jar {
		if len(entry) < 4 {
			continue
		}
		c := mimoLoginCookie{Name: entry[0], Value: entry[1], Domain: entry[2], Path: entry[3]}
		sess.Jar[mimoJarKey(c)] = c
	}
	return sess
}

// mimoSessionFromRequest reads the session straight off the incoming cookie.
func mimoSessionFromRequest(r *http.Request) *mimoLoginSession {
	cookie, err := r.Cookie(mimoSessionCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	return decodeMimoSessionCookie(cookie.Value)
}

func setMimoSessionCookie(w http.ResponseWriter, sess *mimoLoginSession) {
	value := encodeMimoSessionCookie(sess)
	if value == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     mimoSessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(mimoSessionTTL / time.Second),
	})
}

func clearMimoSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     mimoSessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// absorbMimoSetCookies folds one response's Set-Cookie headers into the jar,
// honouring Domain/Path so the same cookie name on the cluster host does not
// overwrite the account host's.
func absorbMimoSetCookies(sess *mimoLoginSession, resp *http.Response, requestURL string) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return
	}
	now := time.Now()
	for _, c := range resp.Cookies() {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain == "" {
			domain = u.Hostname()
		}
		path := c.Path
		if path == "" {
			path = mimoDefaultCookiePath(u.Path)
		}
		key := mimoJarKey(mimoLoginCookie{Name: c.Name, Domain: domain, Path: path})
		expired := c.Value == "" || c.Value == "EXPIRED" || c.MaxAge < 0 ||
			(!c.Expires.IsZero() && c.Expires.Before(now))
		if expired {
			delete(sess.Jar, key)
			continue
		}
		sess.Jar[key] = mimoLoginCookie{Name: c.Name, Value: c.Value, Domain: domain, Path: path}
	}
}

// readMimoSessionIdentity pulls the identity cookies out of the jar. A session
// without a live passToken has not authorized anything yet.
func readMimoSessionIdentity(sess *mimoLoginSession) *mimoIdentity {
	pick := func(name string) string {
		for _, c := range sess.Jar {
			if c.Name == name && mimoDomainMatch(c.Domain, mimoAccountHost) {
				return c.Value
			}
		}
		return ""
	}
	passToken := pick("passToken")
	if passToken == "" || passToken == "EXPIRED" {
		return nil
	}
	return &mimoIdentity{PassToken: passToken, UserID: pick("userId"), CUserID: pick("cUserId")}
}

// defaultMimoHop walks one leg of the login redirect chain without following
// it: the Location header is the next hop, not a place the client may jump to
// on its own.
func defaultMimoHop(ctx context.Context, sess *mimoLoginSession, target, ua string) (mimoHopResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return mimoHopResult{}, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/json,*/*")
	if cookie := sess.cookieHeader(target); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	transport := &http.Transport{}
	if sess.ProxyURL != "" {
		if proxyURL, err := url.Parse(sess.ProxyURL); err == nil && proxyURL.Host != "" {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return mimoHopResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	absorbMimoSetCookies(sess, resp, target)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return mimoHopResult{Status: resp.StatusCode, Location: resp.Header.Get("Location")}, nil
}

// mimoLocalProxyPorts are the default local HTTP proxy ports of the clients
// overseas users reach the login page through (v2rayN, clash, generic).
var mimoLocalProxyPorts = []int{10808, 10809, 7890, 7891, 1080, 1081, 8080, 8888}

// probeMimoLocalProxy returns the first local port that answers a CONNECT to
// the account host, or "" when none does.
func probeMimoLocalProxy(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	found := make(chan string, len(mimoLocalProxyPorts))
	for _, port := range mimoLocalProxyPorts {
		go func(port int) {
			if mimoProxyAnswers(probeCtx, port) {
				found <- fmt.Sprintf("http://127.0.0.1:%d", port)
			}
		}(port)
	}
	select {
	case proxyURL := <-found:
		return proxyURL
	case <-probeCtx.Done():
		return ""
	}
}

func mimoProxyAnswers(ctx context.Context, port int) bool {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte("CONNECT account.xiaomi.com:443 HTTP/1.1\r\nHost: account.xiaomi.com:443\r\n\r\n")); err != nil {
		return false
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return false
	}
	line := string(buf[:n])
	return strings.HasPrefix(line, "HTTP/1.1 200") || strings.HasPrefix(line, "HTTP/1.0 200")
}

// mimoOriginOf is the browser-facing origin of this request. The Host header
// wins: the bind address (0.0.0.0) must never leak into a same-origin URL.
func mimoOriginOf(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	host := r.Header.Get("Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return "http://localhost:20131"
	}
	return proto + "://" + host
}

// mimoPagePath turns the account host's absolute Location into the same-origin
// path the browser loads. Cluster base URLs inside it are rewritten into the
// takeover prefix so the proxied login page keeps its own API calls on this
// origin.
func mimoPagePath(location, origin string) string {
	base, err := url.Parse("https://" + mimoAccountHost)
	if err != nil {
		return location
	}
	page, err := base.Parse(location)
	if err != nil {
		return location
	}
	out := page.Path
	if page.RawQuery != "" {
		out += "?" + page.RawQuery
	}
	for _, upstream := range mimoRegionBases {
		out = strings.ReplaceAll(out, upstream, origin+"/__mimo_login/mimo")
	}
	return out
}

func (h *OAuthHandler) mimoHopFn() func(context.Context, *mimoLoginSession, string, string) (mimoHopResult, error) {
	if h.mimoHop != nil {
		return h.mimoHop
	}
	return defaultMimoHop
}

func (h *OAuthHandler) mimoPollFn() func(*mimoLoginSession) *mimoIdentity {
	if h.mimoPoll != nil {
		return h.mimoPoll
	}
	return readMimoSessionIdentity
}

func (h *OAuthHandler) mimoProxyProbeFn() func(context.Context) string {
	if h.mimoProxyProbe != nil {
		return h.mimoProxyProbe
	}
	return probeMimoLocalProxy
}

// HandleMimoLoginStart walks the first two hops of MiMo Desktop's login surface
// and hands the browser a same-origin page URL plus the session cookie every
// later poll and proxied request rides on.
//
// POST /api/oauth/xiaomi-mimo/login/start
// Body: {region?: "cn"|"sgp"|"ams"|"ru"|"in"}
func (h *OAuthHandler) HandleMimoLoginStart(w http.ResponseWriter, r *http.Request) {
	region := "cn"
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(body) > 0 {
		var req struct {
			Region string `json:"region"`
		}
		if json.Unmarshal(body, &req) == nil {
			if v := strings.ToLower(strings.TrimSpace(req.Region)); mimoLoginRegions[v] {
				region = v
			}
		}
	}
	_ = r.Body.Close()

	sess := newMimoLoginSession(region)
	if region != "cn" {
		// Non-CN clusters may need an overseas exit for the login page's
		// geo-decided features (Google sign-in among them). CN never proxies.
		sess.ProxyURL = strings.TrimSpace(os.Getenv("MIMO_LOGIN_PROXY"))
		if sess.ProxyURL == "" {
			sess.ProxyURL = h.mimoProxyProbeFn()(r.Context())
		}
	}

	hop := h.mimoHopFn()
	meURL := sess.UpstreamBase + "/api/user/xiaomi/me"
	meRes, err := hop(r.Context(), sess, meURL, mimoAPIUA)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !strings.Contains(meRes.Location, mimoAccountHost) {
		handlerutil.WriteJSONError(w, http.StatusBadGateway,
			fmt.Sprintf("Unexpected me response (%d) — no account redirect", meRes.Status))
		return
	}

	loginRes, err := hop(r.Context(), sess, meRes.Location, mimoSSOUA)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if loginRes.Location == "" {
		handlerutil.WriteJSONError(w, http.StatusBadGateway,
			fmt.Sprintf("Unexpected serviceLogin response (%d)", loginRes.Status))
		return
	}

	pageURL := mimoPagePath(loginRes.Location, mimoOriginOf(r))
	setMimoSessionCookie(w, sess)
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"state":   sess.State,
		"pageUrl": pageURL,
		"region":  sess.Region,
	})
}

// HandleMimoLoginStatus reports whether the login session has captured an
// identity yet. Pending re-arms the cookie on every poll — the modal may sit on
// the login form longer than the cookie's TTL.
//
// GET /api/oauth/xiaomi-mimo/login/status?state=...
func (h *OAuthHandler) HandleMimoLoginStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	sess := mimoSessionFromRequest(r)
	if sess == nil || (state != "" && sess.State != state) {
		handlerutil.WriteJSON(w, http.StatusNotFound, map[string]any{"status": "expired"})
		return
	}

	identity := h.mimoPollFn()(sess)
	if identity == nil {
		setMimoSessionCookie(w, sess)
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "pending",
			"region": sess.Region,
		})
		return
	}

	// One-shot: the identity must not linger past the client reading it.
	clearMimoSessionCookie(w)
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":    "done",
		"region":    sess.Region,
		"passToken": identity.PassToken,
		"userId":    nullableStr(identity.UserID),
		"cUserId":   nullableStr(identity.CUserID),
	})
}
