package dashboard_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestDashboard wires a real (mock-upstream) stack behind the dashboard:
// one pooled token by default, or bridge mode when tokens is 0.
func newTestDashboard(t *testing.T, tokens int) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, tokens),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clients := make([]*upstream.Client, 0, tokens)
	sessions := make([]*session.Manager, 0, tokens)
	for i := 0; i < tokens; i++ {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	ts := httptest.NewServer(d.Page("overview"))
	t.Cleanup(ts.Close)
	return ts
}

func TestPageOverviewFull(t *testing.T) {
	ts := newTestDashboard(t, 0) // bridge mode: no fixed tokens
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"<html", "Overview", "Bridge mode", "models"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestPageOverviewFragment(t *testing.T) {
	ts := newTestDashboard(t, 1) // pooled mode: one fixed token
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	frag := string(mustReadAll(t, resp))
	if strings.Contains(frag, "<html") {
		t.Error("htmx request rendered a full page, want bare fragment")
	}
	for _, want := range []string{"Token 0", "low", "session"} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

func TestLoginPageRendersError(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.RenderLogin(rec, httptest.NewRequest(http.MethodGet, "/admin/login", nil), "Invalid admin token.")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"Sign in", "Invalid admin token."} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("login page missing %q", want)
		}
	}
}

// newDashboardForPages builds a dashboard with a wired log ring, mounting the
// given page (defaults to "logs").
func newDashboardForPages(t *testing.T, withRing bool, page ...string) *httptest.Server {
	t.Helper()
	name := "logs"
	if len(page) > 0 && page[0] != "" {
		name = page[0]
	}
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var ring *logring.Handler
	if withRing {
		ring = logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
		slog.New(ring).Info("hello ring", "k", "v")
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, ring)
	ts := httptest.NewServer(d.Page(name))
	t.Cleanup(ts.Close)
	return ts
}

func TestLogsPageWithRing(t *testing.T) {
	ts := newDashboardForPages(t, true)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"hello ring", "ring enabled", "INFO", "k=v"} {
		if !strings.Contains(page, want) {
			t.Errorf("logs page missing %q", want)
		}
	}
}

func TestLogsPageWithoutRing(t *testing.T) {
	ts := newDashboardForPages(t, false)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(string(mustReadAll(t, resp)), "ring disabled") {
		t.Error("logs page should report the ring as disabled")
	}
}

func TestMetricsPageRendersSparklines(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.Page("metrics")(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics", nil))
	page := rec.Body.String()
	for _, want := range []string{"Requests served", "Transient retries", "Fingerprint rotations", "<svg"} {
		if !strings.Contains(page, want) {
			t.Errorf("metrics page missing %q", want)
		}
	}
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The restricted page renders the themed gate (not a bare error line).
func TestRestrictedPageRenders(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.RenderRestricted(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil), "blocked")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	for _, want := range []string{"Restricted", "ADMIN_TOKEN", "blocked"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("restricted page missing %q", want)
		}
	}
}

// The models page renders the catalog with agent mappings.
func TestModelsPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "models")
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"Models", "upstream agent", "z-ai/glm-5.2"} {
		if !strings.Contains(page, want) {
			t.Errorf("models page missing %q", want)
		}
	}
}

// The setup page renders the base URL and client snippets.
func TestSetupPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "setup")
	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"Client setup", "OpenCode", "Continue", "aider", "127.0.0.1", "/v1"} {
		if !strings.Contains(page, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
}

// The traces page renders the recorded chat-trace entry (ring holds a
// non-trace "hello ring" record; the page must not crash and shows the
// empty state when no traces exist).
func TestTracesPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, true, "traces")
	resp, err := http.Get(ts.URL + "/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	if !strings.Contains(page, "No chat traffic yet") {
		t.Error("traces page missing empty state")
	}
}

// --- data-path coverage (AuditServer priority 10) ---

// pageServer builds a dashboard server mounting the named page over a pool
// with the given token count; mut adjusts the config before the stack is
// built, ring wires the log viewer when non-nil. Returns the pool so tests
// can seed cooldowns or assert counters.
func pageServer(t *testing.T, tokens int, page string, mut func(*config.Config), ring *logring.Handler) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, tokens),
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	if mut != nil {
		mut(cfg)
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clients := make([]*upstream.Client, 0, tokens)
	sessions := make([]*session.Manager, 0, tokens)
	for i := 0; i < tokens; i++ {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, ring)
	ts := httptest.NewServer(d.Page(page))
	t.Cleanup(ts.Close)
	return ts, p
}

// dashModel is the fallback-registry model the quota seeding chats use.
const dashModel = "z-ai/glm-5.2"

// quotaPageServer builds a tokens page whose session admission carries the
// given rateLimitsByModel entries, driven through a real pool chat so the
// token snapshot gains QuotaByModel (the only way the quota table renders).
func quotaPageServer(t *testing.T, limits map[string]any) *httptest.Server {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.RateLimitsByModel = limits
	mock.ChatBody = testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, dashModel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	up, err := p.Chat(ctx, lease, upstream.ChatOptions{Model: dashModel, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID},
		[]byte(`{"model":"`+dashModel+`","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, up)
	_ = up.Close()
	p.LeaseRelease(lease)

	ts := httptest.NewServer(d.Page("tokens"))
	t.Cleanup(ts.Close)
	return ts
}

// TestTokensPageQuotaRows pins the quota table: UsagePct clamp at 100,
// NearLimit at >=80, the ResetsIn countdown, and the entitlement cell.
func TestTokensPageQuotaRows(t *testing.T) {
	reset := time.Now().Add(4*time.Hour + 12*time.Minute).UTC().Format(time.RFC3339)
	ts := quotaPageServer(t, map[string]any{
		dashModel: map[string]any{
			"model":       dashModel,
			"limit":       100,
			"recentCount": 120, // > limit → UsagePct clamps to 100
			"period":      "pacific_day",
			"resetAt":     reset,
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
		"deepseek/deepseek-v4-flash": map[string]any{
			"model":       "deepseek/deepseek-v4-flash",
			"limit":       100,
			"recentCount": 80, // exactly at NearLimit threshold
			"period":      "pacific_day",
			"resetAt":     reset,
		},
	})
	resp, err := http.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))

	for _, want := range []string{
		"Session quota by model",
		">120<", // Recent cell of the exhausted row
		">100<", // Limit cell
		">pacific_day<",
		"in 4h 12m",                    // ResetsIn countdown
		"base=1, referral=1, streak=3", // entitlement cell
		"width: 100%",                  // UsagePct clamped from 120
		"usage usage-near",             // NearLimit row styling
		">80<",                         // the 80% row's recent
	} {
		if !strings.Contains(page, want) {
			t.Errorf("tokens page missing %q in:\n%s", want, page)
		}
	}
}

// TestTokensPagePureBridgeInBridgeCard pins the pure-bridge tokens page: the
// InBridge card renders (with the bridge count) and no token table does.
func TestTokensPagePureBridgeInBridgeCard(t *testing.T) {
	ts, _ := pageServer(t, 0, "tokens", nil, nil)
	resp, err := http.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"bridge-card", "Bridge mode", "0 bridge client"} {
		if !strings.Contains(page, want) {
			t.Errorf("bridge tokens page missing %q in:\n%s", want, page)
		}
	}
	if strings.Contains(page, "token-detail") {
		t.Error("pure-bridge tokens page rendered a token table")
	}
}

// TestOverviewPageCooldownCard pins the cooldown-active card: a token whose
// cooldown window is live renders the "cooldown until" row with the RFC3339
// deadline.
func TestOverviewPageCooldownCard(t *testing.T) {
	ts, p := pageServer(t, 1, "overview", nil, nil)
	p.CooldownToken(0, time.Hour)
	resp, err := http.Get(ts.URL + "/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	if !strings.Contains(page, "cooldown until") {
		t.Errorf("overview missing cooldown row in:\n%s", page)
	}
	if !strings.Contains(page, "dot-high") {
		t.Errorf("overview missing high-risk dot for a cooling token in:\n%s", page)
	}
}

// TestModelsPageAliases pins the alias table: MODEL_ALIASES from the config
// render as sorted alias→real rows.
func TestModelsPageAliases(t *testing.T) {
	ts, _ := pageServer(t, 1, "models", func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-4o": dashModel, "sonnet": "anthropic/claude-sonnet-5"}
	}, nil)
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"Aliases", "gpt-4o", dashModel, "sonnet", "anthropic/claude-sonnet-5"} {
		if !strings.Contains(page, want) {
			t.Errorf("models page missing %q in:\n%s", want, page)
		}
	}
}

// TestTracesPageWithLiveTrace pins the chat-trace field parsing: a real
// "chat trace" ring entry renders its token/model/status/ms/error columns.
func TestTracesPageWithLiveTrace(t *testing.T) {
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
	slog.New(ring).Info("chat trace", "token", "1", "model", dashModel, "status", "ok", "ms", 42)
	slog.New(ring).Info("chat trace", "token", "bridge", "model", "deepseek/deepseek-v4-flash", "status", "error", "ms", 7, "error", "upstream")
	ts, _ := pageServer(t, 1, "traces", nil, ring)
	resp, err := http.Get(ts.URL + "/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"42ms", "7ms", dashModel, "trace-ok", "trace-err", "upstream", "bridge"} {
		if !strings.Contains(page, want) {
			t.Errorf("traces page missing %q in:\n%s", want, page)
		}
	}
	if strings.Contains(page, "No chat traffic yet") {
		t.Error("traces page shows the empty state despite live trace entries")
	}
}

// TestSetupPageKeyHintModes pins the setup KeyHint per mode: bridge tells the
// operator the client token IS the upstream credential; hybrid explains the
// relay-or-pool fallback.
func TestSetupPageKeyHintModes(t *testing.T) {
	tsBridge, _ := pageServer(t, 0, "setup", nil, nil)
	resp, err := http.Get(tsBridge.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	bridgePage := string(mustReadAll(t, resp))
	_ = resp.Body.Close()
	for _, want := range []string{"Key:", "the client&#39;s Authorization header IS the upstream token", "switches to pooled mode"} {
		if !strings.Contains(bridgePage, want) {
			t.Errorf("bridge setup page missing %q in:\n%s", want, bridgePage)
		}
	}

	tsHybrid, _ := pageServer(t, 1, "setup", func(c *config.Config) { c.HybridMode = true }, nil)
	resp, err = http.Get(tsHybrid.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	hybridPage := string(mustReadAll(t, resp))
	_ = resp.Body.Close()
	for _, want := range []string{"hybrid mode)", "pill-hybrid"} {
		if !strings.Contains(hybridPage, want) {
			t.Errorf("hybrid setup page missing %q in:\n%s", want, hybridPage)
		}
	}
}

// TestConfigPageEnvAbsentTemplate pins the editor seed: with no .env the
// page reports "no .env yet" and the textarea holds the commented default
// template.
func TestConfigPageEnvAbsentTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, _ := pageServer(t, 0, "config", nil, nil)
	resp, err := http.Get(ts.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"no .env yet", "# freebuff-proxy configuration (.env)", "SAFE_MODE=true", "AUTH_TOKENS=token1,token2"} {
		if !strings.Contains(page, want) {
			t.Errorf("config page missing %q in:\n%s", want, page)
		}
	}
}

// TestConfigPageCRLFVerbatim pins the editor fidelity: a CRLF .env is
// rendered verbatim (the editor must never normalize line endings, or a
// Windows-edited file would be silently rewritten on the next save).
func TestConfigPageCRLFVerbatim(t *testing.T) {
	t.Chdir(t.TempDir())
	crlf := "SAFE_MODE=true\r\nMAX_MESSAGES_PER_DAY=7\r\n"
	if err := os.WriteFile(".env", []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := pageServer(t, 0, "config", nil, nil)
	resp, err := http.Get(ts.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	if !strings.Contains(page, "SAFE_MODE=true\r\nMAX_MESSAGES_PER_DAY=7\r\n") {
		t.Errorf("config page did not render CRLF content verbatim in:\n%s", page)
	}
}

// TestRenderConfigResultFragment pins the htmx fragment path: an HX-Request
// render returns the bare result <p>, not a full page; a plain request
// returns the layout shell.
func TestRenderConfigResultFragment(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	fragReq := httptest.NewRequest(http.MethodPost, "/admin/config", nil)
	fragReq.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	d.RenderConfigResult(rec, fragReq, true, "Saved and reloaded")
	frag := rec.Body.String()
	if strings.Contains(frag, "<html") {
		t.Error("HX-Request config result rendered a full page")
	}
	for _, want := range []string{"result-ok", "Saved and reloaded"} {
		if !strings.Contains(frag, want) {
			t.Errorf("config fragment missing %q: %s", want, frag)
		}
	}

	plainReq := httptest.NewRequest(http.MethodPost, "/admin/config", nil)
	rec = httptest.NewRecorder()
	d.RenderConfigResult(rec, plainReq, false, "rejected")
	plain := rec.Body.String()
	if !strings.Contains(plain, "<html") || !strings.Contains(plain, "rejected") {
		t.Errorf("plain config result = %q, want full page with message", plain)
	}
}

// TestRenderSmokeResultFragment pins the smoke-result htmx fragment: the
// model/token/ms summary and the bounded preview render without a layout.
func TestRenderSmokeResultFragment(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/admin/smoke", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	d.RenderSmokeResult(rec, req, dashModel, "bridge", 123, []byte("preview bytes"))
	frag := rec.Body.String()
	if strings.Contains(frag, "<html") {
		t.Error("HX-Request smoke result rendered a full page")
	}
	for _, want := range []string{"Smoke test OK", dashModel, "bridge", "123ms", "preview bytes"} {
		if !strings.Contains(frag, want) {
			t.Errorf("smoke fragment missing %q: %s", want, frag)
		}
	}
}

// TestConfigPageHTTPProxyMasked verifies that basic auth passwords embedded in
// HTTP_PROXY are masked in the effective config data and never exposed on the page.
func TestConfigPageHTTPProxyMasked(t *testing.T) {
	ts, _ := pageServer(t, 1, "config", func(c *config.Config) {
		c.HTTPProxy = "http://alice:secretPassword123@proxy.internal:8080"
	}, nil)
	resp, err := http.Get(ts.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	if strings.Contains(page, "secretPassword123") {
		t.Errorf("config page exposed raw HTTP_PROXY password: %s", page)
	}
	if !strings.Contains(page, "http://alice:***@proxy.internal:8080") {
		t.Errorf("config page missing masked HTTP_PROXY in: %s", page)
	}
}
