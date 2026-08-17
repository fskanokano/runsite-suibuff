// Package dashboard serves the embedded admin web UI: a single-binary,
// htmx-driven control panel for the proxy (health, config, tokens, logs,
// metrics). Static assets (htmx, pico, app.css) and templates are vendored
// and embedded via go:embed — no runtime CDN, no node build step, and no
// change to the distribution model.
//
// Every page handler renders either a full page (plain requests) or the bare
// content fragment (htmx requests, detected via the HX-Request header), so
// the same URL serves the initial load and the live-updating polls.
package dashboard

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
)

//go:embed assets templates
var files embed.FS

// AssetsFS exposes the vendored static assets (htmx, pico, app.css) for the
// server to mount under /admin/assets/. Embedded paths carry the "assets/"
// prefix, so the server strips "/admin/" before serving.
func AssetsFS() embed.FS { return files }

// Dashboard renders the admin UI over the live pool, registry, and config.
type Dashboard struct {
	cfg     func() *config.Config // returns the current (hot-reloadable) config
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	logs    *logring.Handler // dashboard log viewer source (nil = disabled)
	started time.Time
	tpl     *template.Template

	// metricHist is the rolling counter history sampled by the metrics page
	// (UI-poll-driven, not a background goroutine). Per-instance so multiple
	// dashboards never share one window.
	metricsMu  sync.Mutex
	metricHist []metricSample
}

// New builds the dashboard. cfg must return the current configuration — the
// server passes its atomic pointer loader so /admin/reload is reflected
// immediately. A nil logger falls back to slog.Default(). Template parse
// failures panic: the templates are embedded, so a parse error is a build
// invariant violation, not a runtime condition. logs is the optional log
// viewer ring (nil hides the /admin/logs page data).
func New(cfg func() *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler) *Dashboard {
	if logger == nil {
		logger = slog.Default()
	}
	tpl, err := template.ParseFS(files, "templates/*.tmpl")
	if err != nil {
		panic("dashboard: embedded template parse failed: " + err.Error())
	}
	return &Dashboard{cfg: cfg, pool: p, reg: reg, logger: logger, started: time.Now(), tpl: tpl, logs: logs}
}

// layoutData carries the pre-rendered page body into the layout shell. The
// body is template.HTML deliberately: it was produced by executing one of
// our own escaped content templates, so no second escaping pass applies.
// Page names the content template so the layout can mount the right htmx
// poll for live pages (overview/logs/metrics).
type layoutData struct {
	Body template.HTML
	Page string
}

// isHX reports whether the request came from htmx (fragment, not full page).
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// render executes content (as a bare fragment for htmx requests, or inside
// the layout for plain navigation).
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, content string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHX(r) {
		if err := d.tpl.ExecuteTemplate(w, content, data); err != nil {
			d.logger.Error("dashboard fragment render failed", "template", content, "err", err)
		}
		return
	}
	var buf bytes.Buffer
	if err := d.tpl.ExecuteTemplate(&buf, content, data); err != nil {
		d.logger.Error("dashboard page render failed", "template", content, "err", err)
		return
	}
	if err := d.tpl.ExecuteTemplate(w, "layout", layoutData{Body: template.HTML(buf.String()), Page: content}); err != nil {
		d.logger.Error("dashboard layout render failed", "err", err)
	}
}

// Page returns a handler for the named content template, wired to its data.
func (d *Dashboard) Page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.render(w, r, name, d.dataFor(name))
	}
}

// RenderLogin renders the login page with an optional error message.
func (d *Dashboard) RenderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	d.render(w, r, "login", loginData{Error: errMsg})
}

// RenderRestricted renders the styled access-denied page (the loopback gate
// and sensitive routes use it so blocked pages never look broken).
func (d *Dashboard) RenderRestricted(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHX(r) {
		// htmx polls get a small fragment, not a full page.
		_, _ = w.Write([]byte("<p class=\"result result-err\" role=\"status\">" + template.HTMLEscapeString(msg) + "</p>"))
		return
	}
	var buf bytes.Buffer
	if err := d.tpl.ExecuteTemplate(&buf, "restricted", loginData{Error: msg}); err != nil {
		d.logger.Error("dashboard restricted render failed", "err", err)
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusForbidden)
	if err := d.tpl.ExecuteTemplate(w, "layout", layoutData{Body: template.HTML(buf.String()), Page: "restricted"}); err != nil {
		d.logger.Error("dashboard restricted layout failed", "err", err)
	}
}

// dataFor resolves the page data for a named content template.
func (d *Dashboard) dataFor(name string) any {
	switch name {
	case "overview":
		return d.overviewData()
	case "config":
		return d.configData()
	case "tokens":
		return d.tokensData()
	case "models":
		return d.modelsData()
	case "logs":
		return d.logsData()
	case "traces":
		return d.tracesData()
	case "setup":
		return d.setupData()
	case "metrics":
		return d.metricsData()
	default:
		return nil
	}
}

// --- logs ---

type logsData struct {
	Enabled bool
	Entries []logEntry
}

type logEntry struct {
	Time    string
	Level   string
	Message string
	Fields  string
}

func (d *Dashboard) logsData() logsData {
	ld := logsData{Enabled: d.logs != nil}
	if d.logs == nil {
		return ld
	}
	for _, e := range d.logs.Recent(200) {
		ld.Entries = append(ld.Entries, logEntry{
			Time:    e.Time,
			Level:   e.Level,
			Message: e.Message,
			Fields:  strings.Join(e.Fields, "  "),
		})
	}
	return ld
}

// --- metrics ---

// metricSample is one point of the in-dashboard counter history, sampled on
// every metrics-page render (the page polls every 5s, so sampling is driven
// by the UI, not a background goroutine).
type metricSample struct {
	Requests int64
	Retries  int64
	Rotation int64
}

const maxMetricSamples = 120

type metricsData struct {
	TransientRetries     int64
	FingerprintRotations int64
	RequestsTotal        int64
	Models               int
	SampleCount          int
	RequestsSpark        template.HTML
	RetriesSpark         template.HTML
}

func (d *Dashboard) metricsData() metricsData {
	ps := d.pool.PoolSnapshot()
	md := metricsData{
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		RequestsTotal:        int64(ps.RequestsServed),
		Models:               d.reg.ModelCount(),
	}
	d.metricsMu.Lock()
	d.metricHist = append(d.metricHist, metricSample{Requests: md.RequestsTotal, Retries: ps.TransientRetries, Rotation: ps.FingerprintRotations})
	if len(d.metricHist) > maxMetricSamples {
		d.metricHist = d.metricHist[len(d.metricHist)-maxMetricSamples:]
	}
	hist := make([]metricSample, len(d.metricHist))
	copy(hist, d.metricHist)
	d.metricsMu.Unlock()
	md.SampleCount = len(hist)

	requests := make([]float64, len(hist))
	retries := make([]float64, len(hist))
	for i, s := range hist {
		requests[i] = float64(s.Requests)
		retries[i] = float64(s.Retries)
	}
	md.RequestsSpark = sparklineSVG(requests, "var(--fp-amber)", "requests served over time")
	md.RetriesSpark = sparklineSVG(retries, "var(--fp-teal)", "transient retries over time")
	return md
}

// sparklineSVG renders a normalized polyline sparkline. Values are scaled to
// the chart height (flat when constant or empty). color is an internal CSS
// variable literal; label is the accessible name.
func sparklineSVG(values []float64, color, label string) template.HTML {
	const w, h = 260, 44
	if len(values) < 2 {
		return template.HTML(`<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `"><polyline points="0,` + strconv.Itoa(h-2) + ` ` + strconv.Itoa(w) + `,` + strconv.Itoa(h-2) + `" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`)
	}
	min, max := values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span == 0 {
		span = 1
	}
	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `" preserveAspectRatio="none"><polyline points="`)
	for i, v := range values {
		x := float64(i) * float64(w) / float64(len(values)-1)
		y := float64(h-2) - (v-min)/span*float64(h-4)
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + "," + strconv.FormatFloat(y, 'f', 1, 64))
	}
	sb.WriteString(`" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`)
	return template.HTML(sb.String())
}

// --- tokens ---

type tokensData struct {
	Mode         string // "bridge" | "pooled" | "hybrid"
	InBridge     bool   // tokens page shows the bridge card only in pure bridge
	BridgeTokens int
	TokenCount   int
	Tokens       []tokenDetail
	HasTokens    bool
}

// tokenDetail is the full per-token view: the overview card fields plus the
// live per-model session quota table.
type tokenDetail struct {
	tokenCard
	SessionInstance string
	Quota           []quotaRow
	HasQuota        bool
}

type quotaRow struct {
	Model          string
	Limit          string
	Recent         string
	Period         string
	ResetAt        string
	ResetsIn       string // e.g. "in 4h 12m" (empty when no reset time)
	Entitled       string
	HasEntitlement bool
	UsagePct       int // recent/limit, clamped to 100 (0 when limit is 0)
	NearLimit      bool
	// HasBar reports whether the quota usage bar should render. Limit/Recent
	// are pre-formatted strings (formatQuota), so templates cannot compare
	// them numerically — the numeric decision lives here.
	HasBar bool
}

func (d *Dashboard) tokensData() tokensData {
	cfg := d.cfg()
	td := tokensData{BridgeTokens: d.pool.BridgeCount(), TokenCount: d.pool.TokenCount(), Mode: cfg.EffectiveMode()}
	td.InBridge = td.Mode == "bridge"
	for _, t := range d.pool.Snapshot() {
		detail := tokenDetail{
			tokenCard:       cardFromSnapshot(t),
			SessionInstance: shortID(t.SessionInstanceID),
		}
		for model, q := range t.QuotaByModel {
			row := quotaRow{
				Model:   model,
				Limit:   formatQuota(q.Limit),
				Recent:  formatQuota(q.RecentCount),
				Period:  q.Period,
				ResetAt: shortTime(q.ResetAt),
			}
			if q.Limit > 0 {
				row.UsagePct = int(q.RecentCount * 100 / q.Limit)
				if row.UsagePct > 100 {
					row.UsagePct = 100
				}
				row.NearLimit = row.UsagePct >= 80
				row.HasBar = true
			}
			if !q.ResetAt.IsZero() {
				if d := time.Until(q.ResetAt); d > 0 {
					row.ResetsIn = "in " + humanDuration(d)
				}
			}
			if len(q.Entitlement) > 0 {
				row.Entitled = formatEntitlement(q.Entitlement)
				row.HasEntitlement = true
			}
			detail.Quota = append(detail.Quota, row)
		}
		sort.Slice(detail.Quota, func(i, j int) bool { return detail.Quota[i].Model < detail.Quota[j].Model })
		detail.HasQuota = len(detail.Quota) > 0
		td.Tokens = append(td.Tokens, detail)
	}
	td.HasTokens = len(td.Tokens) > 0
	return td
}

// --- models ---

type modelsData struct {
	Models     []modelRow
	Count      int
	Agents     int
	Aliases    []aliasRow
	HasAliases bool
}

type modelRow struct {
	ID    string
	Agent string
}

type aliasRow struct {
	Alias string
	Real  string
}

func (d *Dashboard) modelsData() modelsData {
	md := modelsData{Count: d.reg.ModelCount(), Agents: len(d.reg.AgentIDs())}
	for _, id := range d.reg.Models() {
		row := modelRow{ID: id}
		if agent, err := d.reg.AgentForModel(id); err == nil {
			row.Agent = agent
		}
		md.Models = append(md.Models, row)
	}
	cfg := d.cfg()
	for alias, real := range cfg.ModelAliases {
		md.Aliases = append(md.Aliases, aliasRow{Alias: alias, Real: real})
	}
	sort.Slice(md.Aliases, func(i, j int) bool { return md.Aliases[i].Alias < md.Aliases[j].Alias })
	md.HasAliases = len(md.Aliases) > 0
	return md
}

// --- traces ---

// tracesData renders the chat-trace ring: recent chat completions and their
// routing outcome (token, model, status, duration, error class).
type tracesData struct {
	Enabled bool
	Traces  []traceEntry
}

type traceEntry struct {
	Time   string
	Token  string
	Model  string
	Status string
	Ms     string
	Error  string
}

func (d *Dashboard) tracesData() tracesData {
	td := tracesData{Enabled: d.logs != nil}
	if d.logs == nil {
		return td
	}
	for _, e := range d.logs.Recent(200) {
		if e.Message != "chat trace" {
			continue
		}
		entry := traceEntry{Time: e.Time, Status: "ok"}
		for _, f := range e.Fields {
			key, value, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch key {
			case "token":
				entry.Token = value
			case "model":
				entry.Model = value
			case "status":
				entry.Status = value
			case "ms":
				entry.Ms = value + "ms"
			case "error":
				entry.Error = value
			}
		}
		if entry.Token == "" {
			entry.Token = "—"
		}
		td.Traces = append(td.Traces, entry)
	}
	return td
}

// --- client setup ---

// setupData renders copy-paste client configuration snippets from the
// effective config (mirrors the -setup command's shapes).
type setupData struct {
	BaseURL      string
	KeyHint      string
	Model        string
	Models       []string
	Mode         string // "bridge" | "pooled" | "hybrid"
	Bridge       bool
	BridgeTokens int
	TokenCount   int
	HasTokens    bool
}

func (d *Dashboard) setupData() setupData {
	cfg := d.cfg()
	host := "localhost"
	if h, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		host = h
	}
	mode := cfg.EffectiveMode()
	sd := setupData{
		BaseURL:      "http://" + host + "/v1",
		Mode:         mode,
		Bridge:       mode == "bridge",
		BridgeTokens: d.pool.BridgeCount(),
		TokenCount:   d.pool.TokenCount(),
		Models:       d.reg.Models(),
	}
	sd.HasTokens = sd.TokenCount > 0
	if len(sd.Models) > 0 {
		sd.Model = sd.Models[0]
	}
	switch mode {
	case "bridge":
		sd.KeyHint = "your FreeBuff token (bridge mode: the client's Authorization header IS the upstream token)"
	case "hybrid":
		sd.KeyHint = "your FreeBuff token — sent when present; otherwise the proxy picks from AUTH_TOKENS (hybrid mode)"
	default:
		sd.KeyHint = "sk-any (pooled mode; the proxy picks from AUTH_TOKENS)"
	}
	return sd
}

// cardFromSnapshot maps one pool snapshot into the shared token-card view
// (overview cards and the tokens-detail header use the same fields).
func cardFromSnapshot(t pool.TokenSnapshot) tokenCard {
	card := tokenCard{
		Index:            t.Token,
		SessionStatus:    t.SessionStatus,
		QueuePosition:    t.SessionQueuePosition,
		QueueDepth:       t.SessionQueueDepth,
		ActiveRuns:       t.ActiveRuns,
		Requests:         t.Requests,
		Messages24h:      t.Messages24h,
		DailyLimit:       t.DailyLimit,
		UsagePct:         t.UsagePct,
		RiskLevel:        t.RiskLevel,
		TransientRetries: t.TransientRetries,
	}
	if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
		card.CooldownActive = true
		card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
	}
	return card
}

// shortID renders a session instance id's first 8 chars for identification.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// formatQuota renders quota numbers without float noise ("5" not "5.0000").
func formatQuota(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatEntitlement(e map[string]float64) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+formatQuota(e[k]))
	}
	return strings.Join(parts, ", ")
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04 Jan 2")
}

// humanDuration renders a duration compactly for countdowns: "4h 12m",
// "45m", "30s". Sub-minute values round up to "1m" so countdowns never
// show a false "0s" while a quota is still active.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		d = time.Minute
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// RenderConfigResult renders the htmx response fragment after a config save
// attempt (success or validation failure).
func (d *Dashboard) RenderConfigResult(w http.ResponseWriter, r *http.Request, ok bool, message string) {
	d.render(w, r, "config_result", configResultData{OK: ok, Message: message})
}

// RenderTestResult appends one per-token outcome to the test-all results
// area (one fragment per token, appended with hx-swap-oob semantics).
func (d *Dashboard) RenderTestResult(w http.ResponseWriter, r *http.Request, token int, ok bool, message, instanceID string) {
	d.render(w, r, "test_result", testResultData{
		Token: token, OK: ok, Message: message, InstanceID: shortID(instanceID),
	})
}

// RenderSmokeResult renders the smoke-test outcome fragment.
func (d *Dashboard) RenderSmokeResult(w http.ResponseWriter, r *http.Request, model, token string, ms int64, preview []byte) {
	d.render(w, r, "smoke_result", smokeResultData{
		Model: model, Token: token, Ms: ms, Preview: string(preview),
	})
}

// RenderDiag renders the diagnostics results fragment.
func (d *Dashboard) RenderDiag(w http.ResponseWriter, r *http.Request, checks []DiagCheck) {
	d.render(w, r, "diag_result", diagData{Checks: checks})
}

// --- overview ---

type overviewData struct {
	Mode                 string // "bridge" | "pooled" | "hybrid"
	InBridge             bool
	BridgeTokens         int
	Models               []string
	ModelCount           int
	Uptime               string
	Rotation             string
	SafeMode             bool
	MaxMessagesPerDay    int
	TransientRetries     int64
	FingerprintRotations int64
	Tokens               []tokenCard
	HasTokens            bool
}

type tokenCard struct {
	Index            int
	SessionStatus    string
	QueuePosition    int
	QueueDepth       int
	ActiveRuns       int
	Requests         int
	Messages24h      int
	DailyLimit       int
	UsagePct         int
	RiskLevel        string
	CooldownActive   bool
	CooldownUntil    string
	TransientRetries int64
}

type loginData struct {
	Error string
}

// --- config editor ---

type configData struct {
	EnvContent string     // current .env text (or a commented template)
	HasEnvFile bool       // whether ./.env existed on disk
	Effective  []configKV // effective values, secrets redacted
}

type configKV struct {
	Key    string
	Value  string
	Secret bool // rendered as a redacted summary, never the raw value
}

type configResultData struct {
	OK      bool
	Message string
}

type testResultData struct {
	Token      int
	OK         bool
	Message    string
	InstanceID string
}

type smokeResultData struct {
	Model   string
	Token   string
	Ms      int64
	Preview string
}

// DiagCheck is one diagnostics row (mirrors -doctor's pass/warn/fail model).
type DiagCheck struct {
	OK      bool
	Warn    bool
	Message string
}

type diagData struct {
	Checks []DiagCheck
}

func (d *Dashboard) configData() configData {
	cfg := d.cfg()
	cd := configData{}
	if raw, err := os.ReadFile(".env"); err == nil {
		cd.HasEnvFile = true
		cd.EnvContent = string(raw)
	} else {
		cd.EnvContent = defaultEnvTemplate
	}
	cd.Effective = []configKV{
		{Key: "LISTEN_ADDR", Value: cfg.ListenAddr},
		{Key: "UPSTREAM_BASE_URL", Value: cfg.UpstreamBaseURL},
		{Key: "AUTH_TOKENS", Value: fmt.Sprintf("%d token(s)", len(cfg.AuthTokens)), Secret: true},
		{Key: "API_KEYS", Value: fmt.Sprintf("%d key(s)", len(cfg.APIKeys)), Secret: true},
		{Key: "ADMIN_TOKEN", Value: boolWord(cfg.AdminToken != ""), Secret: true},
		{Key: "ROTATION_INTERVAL", Value: cfg.RotationInterval.String()},
		{Key: "REQUEST_TIMEOUT", Value: cfg.RequestTimeout.String()},
		{Key: "SESSION_CALL_TIMEOUT", Value: cfg.SessionCallTimeout.String()},
		{Key: "PROXY_ROTATION", Value: cfg.ProxyRotation},
		{Key: "COST_MODE", Value: cfg.CostMode},
		{Key: "TLS_FINGERPRINT", Value: cfg.TLSFingerprint},
		{Key: "REGISTRY_REFRESH", Value: cfg.RegistryRefresh.String()},
		{Key: "DEBUG_DUMP", Value: strconv.FormatBool(cfg.DebugDump)},
		{Key: "LOG_FILE", Value: cfg.LogFile},
		{Key: "LOG_LEVEL", Value: cfg.LogLevel},
		{Key: "MAX_MESSAGES_PER_DAY", Value: strconv.Itoa(cfg.MaxMessagesPerDay)},
		{Key: "IDLE_ROTATION_TIMEOUT", Value: cfg.IdleRotationTimeout.String()},
		{Key: "SAFE_MODE", Value: strconv.FormatBool(cfg.SafeMode)},
		{Key: "HYBRID_MODE", Value: strconv.FormatBool(cfg.HybridMode)},
		{Key: "REQUEST_JITTER", Value: cfg.RequestJitter.String()},
		{Key: "CLI_VERSION", Value: cfg.CLIVersion},
		{Key: "MODEL_ALIASES", Value: fmt.Sprintf("%d alias(es)", len(cfg.ModelAliases)), Secret: true},
		{Key: "TRANSIENT_RETRIES", Value: strconv.Itoa(cfg.TransientRetries)},
		{Key: "HTTP_PROXY", Value: maskProxyURL(cfg.HTTPProxy)},
		{Key: "SOCKS5_PROXY", Value: boolWord(cfg.SOCKS5Proxy != ""), Secret: true},
		{Key: "SOCKS5_PROXIES", Value: fmt.Sprintf("%d proxy(es)", len(cfg.SOCKS5Proxies)), Secret: true},
	}
	return cd
}

// maskProxyURL masks any embedded basic auth password in raw proxy URL.
func maskProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); hasPass {
		schemeIdx := strings.Index(raw, "://")
		if schemeIdx >= 0 {
			rest := raw[schemeIdx+3:]
			if at := strings.LastIndexByte(rest, '@'); at >= 0 {
				userinfo := rest[:at]
				if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
					return raw[:schemeIdx+3+colon+1] + "***" + rest[at:]
				}
			}
		}
		u.User = url.UserPassword(u.User.Username(), "***")
		return u.String()
	}
	return raw
}

func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

// defaultEnvTemplate seeds the editor when no ./.env file exists yet. Values
// mirror the loader defaults; uncomment and edit what you need.
const defaultEnvTemplate = `# freebuff-proxy configuration (.env)
# Keys mirror the environment variables; leave commented to keep the default.
# See the README and docs/guides for the full reference.

#LISTEN_ADDR=127.0.0.1:3457
#UPSTREAM_BASE_URL=https://www.codebuff.com
#AUTH_TOKENS=token1,token2
#API_KEYS=sk-local-...
#ADMIN_TOKEN=change-me
#ROTATION_INTERVAL=6h
#REQUEST_TIMEOUT=15m
#SESSION_CALL_TIMEOUT=30s
#PROXY_ROTATION=per-token
#COST_MODE=free
#TLS_FINGERPRINT=chrome120
#REGISTRY_REFRESH=6h
#DEBUG_DUMP=false
#LOG_FILE=
#LOG_LEVEL=info
#MAX_MESSAGES_PER_DAY=0
#IDLE_ROTATION_TIMEOUT=0
#SAFE_MODE=true
#HYBRID_MODE=false
#REQUEST_JITTER=0s
#CLI_VERSION=0.10.7
#MODEL_ALIASES=
#TRANSIENT_RETRIES=1
#HTTP_PROXY=
#SOCKS5_PROXY=
#SOCKS5_PROXIES=
`

func (d *Dashboard) overviewData() overviewData {
	cfg := d.cfg()
	ps := d.pool.PoolSnapshot()
	mode := cfg.EffectiveMode()
	od := overviewData{
		Mode:                 mode,
		InBridge:             mode == "bridge",
		Models:               d.reg.Models(),
		ModelCount:           d.reg.ModelCount(),
		Uptime:               time.Since(d.started).Round(time.Second).String(),
		Rotation:             cfg.ProxyRotation,
		SafeMode:             cfg.SafeMode,
		MaxMessagesPerDay:    cfg.MaxMessagesPerDay,
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		BridgeTokens:         d.pool.BridgeCount(),
	}
	for _, t := range ps.Tokens {
		od.Tokens = append(od.Tokens, cardFromSnapshot(t))
	}
	od.HasTokens = len(od.Tokens) > 0
	return od
}
