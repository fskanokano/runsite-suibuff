package server_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// dashboardURL returns the base URL of a test server with AdminToken set.
func dashboardServer(t *testing.T, adminToken string, mut func(*config.Config)) *httptest.Server {
	t.Helper()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = adminToken
		if mut != nil {
			mut(c)
		}
	}, testutil.NewMock())
	return ts
}

// noRedirectClient never follows redirects, so tests can assert on them.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func get(t *testing.T, url, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postLogin(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("token="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cookie's Secure flag follows the login transport: HTTPS (or a
// TLS-terminating proxy via X-Forwarded-Proto) sets it, plain HTTP does not
// (a Secure cookie over plain HTTP would be rejected and break login).
func TestDashboardCookieSecureFollowsTransport(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	plain := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = plain.Body.Close() }()
	if c := plain.Cookies(); len(c) == 1 && c[0].Secure {
		t.Error("plain-HTTP login set a Secure cookie")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader("token=secret"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	c := resp.Cookies()
	if len(c) != 1 || !c[0].Secure {
		t.Error("X-Forwarded-Proto: https login did not set a Secure cookie")
	}
}

// The dashboard is open when ADMIN_TOKEN is unset (legacy behavior).
func TestDashboardOpenWithoutAdminToken(t *testing.T) {
	ts := dashboardServer(t, "", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open dashboard)", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Overview") {
		t.Error("dashboard page missing overview heading")
	}
}

// Without a session cookie the dashboard redirects to the login page.
func TestDashboardRedirectsToLogin(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("redirect location = %q, want /admin/login", loc)
	}
}

// Login flow: wrong token rejected, right token issues a cookie that unlocks
// the dashboard, and the cookie is HttpOnly + SameSite=Strict.
func TestDashboardLoginFlow(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	// Wrong token: page re-rendered with an error, no cookie set.
	resp := postLogin(t, ts.URL+"/admin/login", "wrong")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-token status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Invalid admin token.") {
		t.Error("wrong-token response missing error message")
	}
	if c := resp.Cookies(); len(c) != 0 {
		t.Errorf("wrong token set cookies: %v", c)
	}

	// Correct token: redirect to /admin plus the session cookie.
	resp = postLogin(t, ts.URL+"/admin/login", "secret")
	// Drain and close the redirect body (it has none) so the connection is
	// released back to the client pool instead of leaking per run.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("login redirect = %q, want /admin", loc)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags wrong: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}

	// The cookie unlocks /admin.
	authed := get(t, ts.URL+"/admin", c.Name+"="+c.Value)
	defer func() { _ = authed.Body.Close() }()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d, want 200", authed.StatusCode)
	}
	if !strings.Contains(bodyOf(t, authed), "Overview") {
		t.Error("authed dashboard missing overview heading")
	}
}

// Tampered cookies are rejected (HMAC validation).
func TestDashboardRejectsTamperedCookie(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	// A syntactically valid-looking but unsigned cookie value.
	resp := get(t, ts.URL+"/admin", "fb_admin=9999999999.deadbeef")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("tampered-cookie status = %d, want 302 redirect to login", resp.StatusCode)
	}
}

// lockoutBound matches server.maxLoginFails (5), pinned by
// TestAdminAuthLockoutBound in auth_internal_test.go; this test drives the
// HTTP surface with one more attempt than the bound.
const lockoutBound = 5

// Assets are public (the login page loads them without a cookie).
func TestDashboardAssetsPublic(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin/assets/pico.min.css", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200 without a cookie", resp.StatusCode)
	}
}

// With ADMIN_TOKEN unset, secret-bearing routes (config) require a loopback
// client; a remote client gets 403, not the .env.
func TestDashboardConfigLoopbackGate(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	// Pin a non-loopback client explicitly: the loopback gate reads
	// RemoteAddr, and httptest.NewRequest's default (192.0.2.1) is
	// undocumented behavior a toolchain change could flip.
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote config status = %d, want 403", rec.Code)
	}
}

// htmx polls get 401 + HX-Redirect on an expired/missing session, not a 302
// that would swap a login fragment into the dashboard.
func TestDashboardHXAuthRedirect(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("hx status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/admin/login" {
		t.Fatalf("HX-Redirect = %q, want /admin/login", got)
	}
}

func TestDashboardLoginRateLimit(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	for range lockoutBound + 1 {
		resp := postLogin(t, ts.URL+"/admin/login", "wrong")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Too many failed attempts") {
		t.Error("rate-limited login did not show lockout message")
	}
}

// --- config editor ---

// postConfig submits .env content to the authed config endpoint.
func postConfig(t *testing.T, url, cookie, content string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/admin/config", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// authedCookie logs into the test dashboard and returns the session cookie.
func authedCookie(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login issued %d cookies, want 1", len(cookies))
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

// Token actions: unlock/finish/test endpoints work with a session cookie.
func TestDashboardTokenActions(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Unlock a token that has no lock — the action is idempotent success.
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/unlock")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token 0 unlocked") {
		t.Errorf("unlock response = %q, want success message", body)
	}

	// Out-of-range token fails cleanly.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/9/unlock")
	if !strings.Contains(bodyOf(t, resp), "out of range") {
		t.Error("out-of-range unlock did not report failure")
	}

	// Finish runs: succeeds (mock has no runs to finish).
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/finish")
	if !strings.Contains(bodyOf(t, resp), "runs finished") {
		t.Error("finish action did not report success")
	}

	// Test: zero-cost upstream probe against the mock (no session claim).
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/test")
	if !strings.Contains(bodyOf(t, resp), "zero-cost probe succeeded") {
		t.Error("test action did not report success")
	}
}

func doTokenAction(t *testing.T, url, cookie, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Runtime token management endpoints: add, remove, mode switch, persisted to
// .env (isolated via t.Chdir).
func TestDashboardTokenAddRemoveMode(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Add a token: pool grows, .env updated.
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_newtoken123"}`)
	if !strings.Contains(bodyOf(t, resp), "Token added") {
		t.Errorf("add response = %q, want success", bodyOf(t, resp))
	}
	env, _ := os.ReadFile(".env")
	if !strings.Contains(string(env), "cb_newtoken123") {
		t.Error("added token not persisted to .env")
	}

	// Mode switch to bridge: pool empties, AUTH_TOKENS cleared.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	if !strings.Contains(bodyOf(t, resp), "Switched to bridge mode") {
		t.Errorf("mode response = %q, want bridge switch", bodyOf(t, resp))
	}
	env, _ = os.ReadFile(".env")
	if strings.Contains(string(env), "cb_newtoken123") {
		t.Error("token still in .env after bridge switch")
	}
}

// TestDashboardModeSwitchClearsJSONConfigTokens is the regression for the
// reported bug "EXE still shows bridge mode false after clicking": when
// AUTH_TOKENS come from a -config JSON file (common for the Windows binary),
// the mode switch writes AUTH_TOKENS= into .env, and the reload must land in
// bridge mode — the empty .env value has to clear the JSON-provided tokens.
// The pool must also be drained only after the reload verifies bridge mode.
func TestDashboardModeSwitchClearsJSONConfigTokens(t *testing.T) {
	t.Chdir(t.TempDir())

	// A -config JSON supplying the tokens, exactly how the EXE is run.
	if err := os.WriteFile("config.json", []byte(`{"AUTH_TOKENS":["tok-0"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "config.json")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Switched to bridge mode") {
		t.Fatalf("mode response = %q, want bridge switch", body)
	}

	// The reload must now be in bridge mode: empty AUTH_TOKENS= in .env
	// beats the JSON list, so BridgeMode() is true on a fresh Load too.
	env, _ := os.ReadFile(".env")
	if strings.Contains(string(env), "tok-0") {
		t.Error("token still in .env after bridge switch")
	}
	reloaded, err := config.Load("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.BridgeMode() {
		t.Error("reloaded config not in bridge mode: JSON tokens not cleared by empty .env AUTH_TOKENS")
	}
	if got := p.TokenCount(); got != 0 {
		t.Errorf("pool TokenCount = %d, want 0 after bridge switch", got)
	}
}

// TestDashboardModeSwitchToHybridPersistsEnv verifies the hybrid mode switch
// persists HYBRID_MODE=true (keeping AUTH_TOKENS), the reload lands in hybrid
// mode (healthz "mode"), pooled and bridge switches clear HYBRID_MODE, and a
// bridge switch still empties the tokens.
func TestDashboardModeSwitchToHybridPersistsEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Add a token so hybrid has a pool to fall back on.
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_hybrid123"}`)
	if !strings.Contains(bodyOf(t, resp), "Token added") {
		t.Fatalf("add response = %q, want success", bodyOf(t, resp))
	}

	// Pooled → hybrid: HYBRID_MODE=true persisted, tokens kept.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"hybrid"}`)
	if !strings.Contains(bodyOf(t, resp), "Switched to hybrid mode") {
		t.Fatalf("hybrid response = %q, want hybrid switch", bodyOf(t, resp))
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "HYBRID_MODE=true") {
		t.Errorf(".env missing HYBRID_MODE=true: %s", env)
	}
	if !strings.Contains(string(env), "cb_hybrid123") {
		t.Errorf("hybrid switch must keep AUTH_TOKENS: %s", env)
	}

	// The live config reports hybrid via healthz.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var hz struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &hz); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if hz.Mode != "hybrid" {
		t.Errorf("healthz mode = %q, want hybrid", hz.Mode)
	}

	// Hybrid → pooled: HYBRID_MODE=false, tokens kept.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"pooled"}`)
	if !strings.Contains(bodyOf(t, resp), "Switched to pooled mode") {
		t.Fatalf("pooled response = %q, want pooled switch", bodyOf(t, resp))
	}
	env, _ = os.ReadFile(".env")
	if !strings.Contains(string(env), "HYBRID_MODE=false") {
		t.Errorf(".env missing HYBRID_MODE=false after pooled switch: %s", env)
	}
	if !strings.Contains(string(env), "cb_hybrid123") {
		t.Errorf("pooled switch must keep AUTH_TOKENS: %s", env)
	}

	// Pooled → bridge: HYBRID_MODE=false AND tokens cleared.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	if !strings.Contains(bodyOf(t, resp), "Switched to bridge mode") {
		t.Fatalf("bridge response = %q, want bridge switch", bodyOf(t, resp))
	}
	env, _ = os.ReadFile(".env")
	if strings.Contains(string(env), "HYBRID_MODE=true") {
		t.Errorf("HYBRID_MODE still true after bridge switch: %s", env)
	}
	if !strings.Contains(string(env), "HYBRID_MODE=false") {
		t.Errorf(".env missing HYBRID_MODE=false after bridge switch: %s", env)
	}
	if strings.Contains(string(env), "cb_hybrid123") {
		t.Errorf("token still in .env after bridge switch: %s", env)
	}
}

func postJSON(t *testing.T, url, cookie, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestDashboardTokensPageHybrid verifies the tokens page renders the hybrid
// pill, the hybrid mode-card, and the "Switch to hybrid mode" button when the
// proxy runs in hybrid mode (pooled-style token table + hybrid labels).
func TestDashboardTokensPageHybrid(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", func(c *config.Config) {
		c.HybridMode = true
		c.AuthTokens = []string{"tok-0"}
	})
	cookie := authedCookie(t, ts)
	resp := get(t, ts.URL+"/admin/tokens", cookie)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tokens page status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"pill-hybrid", ">hybrid<", "Hybrid mode", "Switch to hybrid mode", "Switch to bridge mode"} {
		if !strings.Contains(body, want) {
			t.Errorf("tokens page missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bridge-card") {
		t.Error("tokens page renders the bridge card in hybrid mode, want pooled-style table")
	}
}

// A valid .env save persists the file and reports success.
func TestDashboardConfigSave(t *testing.T) {
	t.Chdir(t.TempDir())
	// Seed a prior .env with an unrelated key to prove the editor replaces
	// the file wholesale (not merge).
	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# my config\nMAX_MESSAGES_PER_DAY=7\nSAFE_MODE=true\n"
	resp := postConfig(t, ts.URL, cookie, content)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Saved and reloaded") {
		t.Error("save response missing success class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env after save = %q, want %q", got, content)
	}
}

// A rejected save restores the previous .env content (rollback).
func TestDashboardConfigSaveRejectedRollsBack(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "SAFE_MODE=true\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate — the save must be rejected
	// and the file restored.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Configuration rejected") {
		t.Error("rejected save missing error class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after rejected save = %q, want original %q", got, original)
	}
}

// TestDashboardConfigSaveRejectedUnreadableEnv pins the rollback guard for a
// present-but-unreadable .env: os.ReadFile fails for reasons other than
// absence (permissions, ACL), so the rollback must NOT remove the file —
// deleting it would destroy the operator's .env even though writeFileAtomic
// (temp + rename) could overwrite it. The rejected save leaves the newly
// written content in place and reports the rejection. POSIX-only: chmod 000
// does not block reads on Windows.
func TestDashboardConfigSaveRejectedUnreadableEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not make a file unreadable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	t.Chdir(t.TempDir())
	// Present-but-unreadable: a file ReadFile cannot open, while the
	// rename-over in writeFileAtomic still succeeds (only the directory needs
	// write permission).
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(".env", 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(".env"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup: ReadFile = %v, want a non-NotExist error", err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate — the save is rejected after
	// the .env was overwritten; the rollback must leave the file in place.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Configuration rejected") {
		t.Error("rejected save missing error class")
	}
	if _, statErr := os.Stat(".env"); statErr != nil {
		t.Errorf(".env deleted by the rollback of an unreadable original: %v", statErr)
	}
}

// The config page renders the effective values with secrets redacted.
func TestDashboardConfigPage(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	resp := get(t, ts.URL+"/admin/config", cookie)
	defer func() { _ = resp.Body.Close() }()
	page := bodyOf(t, resp)
	for _, want := range []string{"Effective configuration", "LISTEN_ADDR", "AUTH_TOKENS", "Editor", "Save &amp; reload"} {
		if !strings.Contains(page, want) {
			t.Errorf("config page missing %q", want)
		}
	}
}

// A CRLF .env stays CRLF after a token add: updateAuthTokensEnv must not
// rewrite a Windows-edited file with mixed line endings.
func TestDashboardTokenAddPreservesCRLF(t *testing.T) {
	t.Chdir(t.TempDir())
	seed := "SAFE_MODE=true\r\nMAX_MESSAGES_PER_DAY=7\r\n"
	if err := os.WriteFile(".env", []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_crlf_token"}`)
	if !strings.Contains(bodyOf(t, resp), "Token added") {
		t.Errorf("add response = %q, want success", bodyOf(t, resp))
	}

	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "cb_crlf_token") {
		t.Error("added token missing from .env")
	}
	if !strings.Contains(string(env), "SAFE_MODE=true\r\n") {
		t.Error("seed line lost CRLF")
	}
	// No bare \n outside a \r\n pair: the file must be uniformly CRLF.
	if strings.Contains(strings.ReplaceAll(string(env), "\r\n", ""), "\n") {
		t.Errorf("mixed line endings after token add:\n%s", env)
	}
}

// Concurrent token adds must not lose updates: adminSaveMu serializes the
// cfg read + .env write + reload, so every added token lands in .env, the
// live pool, and the reloaded config — a lost pool token with a correct .env
// (or vice versa) must fail here.
func TestDashboardConcurrentTokenAdds(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("cb_conc_%d", i)
			resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"`+token+`"}`)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		want := fmt.Sprintf("cb_conc_%d", i)
		if !strings.Contains(string(env), want) {
			t.Errorf("token %q lost from .env after concurrent adds", want)
		}
	}
	// All 1+8 tokens must be in the live pool...
	if got := p.TokenCount(); got != n+1 {
		t.Errorf("pool TokenCount = %d, want %d", got, n+1)
	}
	// ...and a fresh config.Load must agree with both (.env is the source
	// of truth for cfg after each add's reload).
	reloaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.AuthTokens); got != n+1 {
		t.Errorf("reloaded AUTH_TOKENS = %d, want %d", got, n+1)
	}
}

// postForm submits a urlencoded form to an admin POST endpoint (the browser
// dashboard's native wire format).
func postForm(t *testing.T, url, cookie, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A browser urlencoded save must decode the textarea's "content" field; a
// raw urlencoded body written verbatim as .env would destroy the file
// ("content=KEY=VALUE..."). The reload must also succeed on the decoded
// values.
func TestDashboardConfigSaveURLEncoded(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# my config\nMAX_MESSAGES_PER_DAY=7\nSAFE_MODE=true\n"
	resp := postForm(t, ts.URL, cookie, "/admin/config", url.Values{"content": {content}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Saved and reloaded") {
		t.Error("urlencoded save response missing success class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env after urlencoded save = %q, want decoded %q (not \"content=...\")", got, content)
	}
}

// The smoke form posts urlencoded model=&prompt=; the handler must read the
// form (like handleTokenAdd), not json.Unmarshal the raw body. JSON clients
// must keep working.
func TestDashboardSmokeFormAndJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp := postForm(t, ts.URL, cookie, "/admin/smoke", url.Values{"model": {modelA}, "prompt": {"ping"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke form status = %d, want 200", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "Smoke test OK") {
		t.Errorf("smoke form response = %q, want success fragment", body)
	}

	resp = postJSON(t, ts.URL, cookie, "/admin/smoke", `{"model":"`+modelA+`","prompt":"ping"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke JSON status = %d, want 200", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "Smoke test OK") {
		t.Errorf("smoke JSON response = %q, want success fragment", body)
	}
}

// Cross-origin admin POSTs must be rejected (CSRF): a browser on another
// origin sends Origin (and/or Sec-Fetch-Site) on every request, while curl
// and API clients send neither. Matching Origin and no-header requests pass.
func TestDashboardCSRF(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Cross-origin Origin → 403 with the rejection fragment.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_evil"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "Cross-origin request rejected.") {
		t.Errorf("cross-origin body = %q, want rejection message", body)
	}

	// Cross-site Sec-Fetch-Site (no Origin) → 403.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_evil2"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site Sec-Fetch-Site POST status = %d, want 403", resp.StatusCode)
	}

	// Matching Origin (the proxy's own authority) → passes.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_same"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts.URL)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("same-origin POST body = %q, want success (matching Origin must pass)", body)
	}

	// No Origin/Sec-Fetch-Site headers (curl, API clients) → passes.
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_csrf_curl"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("no-header POST body = %q, want success (header-less clients must pass)", body)
	}
}

// After a config-editor AUTH_TOKENS edit, cfg.AuthTokens diverges from the
// live pool; removing "the last token" from the stale list must be rejected
// instead of persisting a wrong .env.
func TestDashboardTokenRemoveRejectsDiverged(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil) // 1 pooled token
	cookie := authedCookie(t, ts)

	// Config editor lists two tokens; the pool still holds one.
	resp := postConfig(t, ts.URL, cookie, "AUTH_TOKENS=tok-0,extra-token\nSAFE_MODE=true\n")
	if body := bodyOf(t, resp); !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("config save failed: %s", body)
	}

	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove status = %d, want 200 with rejection message", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "differs from the live pool") {
		t.Errorf("remove response = %q, want divergence rejection", body)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "extra-token") {
		t.Error("diverged .env must be left untouched")
	}
}

// A failed persist after removal must roll the pool back (mirroring
// handleTokenAdd): the token is re-added so pool/.env/cfg stay consistent.
func TestDashboardTokenRemoveRollsBackOnPersistFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	// Seed an invalid .env: the post-removal reload fails Validate, so
	// syncTokensAfterMutation errors and the pool must re-add the token.
	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "reload config") {
		t.Errorf("remove response = %q, want reload failure surfaced", body)
	}
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount after failed remove = %d, want 1 (rollback re-added the token)", got)
	}
}

// handleDiag must not append ":443" to an UpstreamBaseURL that already
// carries a port: the mock URL (http://127.0.0.1:PORT) is dialed as-is and
// reported reachable (a host that already has a port must never become
// "host:PORT:443"). The DNS row must also strip the port: LookupHost of
// "127.0.0.1:PORT" would treat the whole string as a DNS name and NXDOMAIN,
// a false red row next to a green TCP row (regression for the P3 finding).
func TestDashboardDiagPortHandling(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/diag", "{}")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diag status = %d, want 200", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "TCP reachable 127.0.0.1:") {
		t.Errorf("diag did not dial the ported mock host:\n%s", body)
	}
	if !strings.Contains(body, "DNS resolves 127.0.0.1") {
		t.Errorf("diag DNS row used the host:port as a DNS name:\n%s", body)
	}
	if strings.Contains(body, "DNS lookup failed") {
		t.Errorf("diag DNS row failed for a host with an explicit port:\n%s", body)
	}
	if strings.Contains(body, ":443") {
		t.Errorf("diag appended :443 to a host that already carries a port:\n%s", body)
	}
}

// A config save that adds MODEL_ALIASES must apply to live alias resolution
// (registry SetConfig wiring): a chat with the alias reaches upstream with
// the resolved model, no restart needed.
func TestDashboardConfigSaveAppliesModelAliases(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postConfig(t, ts.URL, cookie, "AUTH_TOKENS=tok-0\nMODEL_ALIASES=gpt-4o:"+modelA+"\n")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("config save failed: %s", body)
	}

	resp2, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("gpt-4o"), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chat with alias status = %d, want 200: %s", resp2.StatusCode, data)
	}
	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no chat requests recorded upstream")
	}
	last := mock.RecordedChatBodies[len(mock.RecordedChatBodies)-1]
	if !strings.Contains(last, modelA) {
		t.Errorf("upstream body = %s, want resolved model %s after config-save alias", last, modelA)
	}
}
