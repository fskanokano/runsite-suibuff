package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/testutil"
)

// testConfig builds a config; baseURL "" keeps the default (only for tests
// that do not perform requests). All request-making tests pass mock.URL().
func testConfig(baseURL string, mut func(*config.Config)) *config.Config {
	cfg := &config.Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok-a"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	if baseURL != "" {
		cfg.UpstreamBaseURL = baseURL
	}
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

// TestProxyURLErrorsRedactCredentials verifies malformed proxy URLs never
// leak the userinfo password into error messages (parse errors are logged
// verbatim at startup and in the egress probe).
func TestProxyURLErrorsRedactCredentials(t *testing.T) {
	const (
		socks = "socks5://alice:s3cret@"
		httpP = "http://alice:s3cret@: bad"
		bad   = "s3cret"
	)

	if _, _, err := parseSocks5(socks); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("parseSocks5(%q) err = %v, want a redacted no-host error", socks, err)
	}

	if _, err := New("tok", testConfig("", func(c *config.Config) { c.SOCKS5Proxy = socks })); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("SOCKS5_PROXY=%q err = %v, want a redacted error", socks, err)
	}
	if _, err := New("tok", testConfig("", func(c *config.Config) { c.HTTPProxy = httpP })); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("HTTP_PROXY=%q err = %v, want a redacted error", httpP, err)
	}

	// The redactor itself: password masked, username kept, no-userinfo and
	// unparseable URLs passed through untouched.
	if got := redactProxyURL("socks5://alice:s3cret@127.0.0.1:1080"); got != "socks5://alice:***@127.0.0.1:1080" {
		t.Errorf("redactProxyURL = %q, want password masked", got)
	}
	if got := redactProxyURL("socks5://127.0.0.1:1080"); got != "socks5://127.0.0.1:1080" {
		t.Errorf("redactProxyURL(no userinfo) = %q, want unchanged", got)
	}
	if got := redactProxyURL("://unparseable"); got != "://unparseable" {
		t.Errorf("redactProxyURL(unparseable) = %q, want unchanged", got)
	}
}

// TestChatCompletionsStreamBodySurvives streams three chunks with real
// delays and asserts the whole body reads back. Regression: do() used to
// defer-cancel the request context when the response headers arrived, which
// aborted every streamed body read (observed live: "upstream stream error:
// context canceled" right after a successful upstream 200).
func TestChatCompletionsStreamBodySurvives(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chunks := []string{
		`{"id":"c0","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"0"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"1"},"finish_reason":null}]}`,
		`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"2"},"finish_reason":null}]}`,
	}
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk))
			flusher.Flush()
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("stream read failed (request context canceled too early?): %v", err)
	}
	text := string(data)
	for i, want := range []string{`"content":"0"`, `"content":"1"`, `"content":"2"`, "[DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q (chunk %d): %s", want, i, text)
		}
	}
}

func TestChatCompletionsEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{
		Model:             "deepseek/deepseek-v4-flash",
		RunID:             "run-abc",
		SessionInstanceID: "inst-1",
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	headers, bodies := mock.RecordedChatHeaders, mock.RecordedChatBodies
	if len(headers) != 1 || len(bodies) != 1 {
		t.Fatalf("want 1 chat request, got %d / %d", len(headers), len(bodies))
	}
	h := headers[0]
	if got := h.Get("x-freebuff-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-model = %q", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "inst-1" {
		t.Errorf("x-freebuff-instance-id = %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer tok-a" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Accept"); got != "application/json, text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("recorded body not JSON: %v", err)
	}
	md, ok := sent["codebuff_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("missing codebuff_metadata in %s", bodies[0])
	}
	if md["run_id"] != "run-abc" {
		t.Errorf("run_id = %v", md["run_id"])
	}
	if md["freebuff_instance_id"] != "inst-1" {
		t.Errorf("freebuff_instance_id = %v", md["freebuff_instance_id"])
	}
	clientID, _ := md["client_id"].(string)
	if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(clientID) {
		t.Errorf("client_id %q not 13-char base36", clientID)
	}
	provider, ok := sent["provider"].(map[string]any)
	if !ok || provider["data_collection"] != "deny" {
		t.Errorf("provider.data_collection not deny: %v", sent["provider"])
	}
	if sent["stream"] != true {
		t.Errorf("stream not forced: %v", sent["stream"])
	}
	stop, ok := sent["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "cb_easp" {
		t.Errorf("stop sentinel not injected: %v", sent["stop"])
	}
	if sent["temperature"] != 0.7 {
		t.Errorf("temperature lost in envelope: %v", sent["temperature"])
	}
	if sent["cost_mode"] != nil {
		// cost_mode lives inside codebuff_metadata only
		t.Errorf("cost_mode leaked to top level: %v", sent["cost_mode"])
	}
}

func TestEnvelopeCostModeAndStopPreserved(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// cost_mode present
	withMode, err := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.CostMode = "free" }))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := withMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	var sent map[string]any
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[0]), &sent)
	md := sent["codebuff_metadata"].(map[string]any)
	if md["cost_mode"] != "free" {
		t.Errorf("cost_mode = %v, want free", md["cost_mode"])
	}

	// cost_mode absent
	noMode, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[1]), &sent)
	md = sent["codebuff_metadata"].(map[string]any)
	if _, present := md["cost_mode"]; present {
		t.Errorf("cost_mode present despite empty config: %v", md)
	}

	// client-supplied stop is preserved
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"},
		[]byte(`{"model":"m","stop":["my-stop"]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[2]), &sent)
	stop := sent["stop"].([]any)
	if len(stop) != 1 || stop[0] != "my-stop" {
		t.Errorf("client stop overwritten: %v", stop)
	}
}

func TestUAIsCLIUserAgent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if got := mock.RecordedChatHeaders[i].Get("User-Agent"); got != cliUserAgent {
			t.Errorf("request %d UA = %q, want the fixed CLI UA %q", i, got, cliUserAgent)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"run invalid", 400, `{"error":"runId not found"}`, ErrRunInvalid},
		{"run not running", 400, `{"error":"runId not running"}`, ErrRunInvalid},
		{"session superseded", 400, `{"error":"session_superseded"}`, ErrSessionInvalid},
		{"session expired", 400, `{"error":"session_expired"}`, ErrSessionInvalid},
		{"update required", 400, `{"error":"freebuff_update_required"}`, ErrSessionInvalid},
		{"auth", 401, `{"error":"unauthorized"}`, ErrAuthRejected},
		{"waiting room 503", 503, `{"error":"waiting_room_queued"}`, ErrWaitingRoom},
		{"waiting room body", 429, `{"error":"waiting_room_required"}`, ErrSessionInvalid},
		{"generic", 500, `{"error":"boom"}`, &UpstreamError{Status: 500}},
		{"402 out of credits", 402, `{"error":"out of credits"}`, ErrCredits},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatStatus = tc.status
			mock.ChatErrorBody = tc.body

			client, err := New("tok", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
			if err == nil {
				t.Fatal("expected error")
			}
			if _, isUpstream := tc.want.(*UpstreamError); isUpstream {
				var upErr *UpstreamError
				if !errors.As(err, &upErr) {
					t.Fatalf("want UpstreamError, got %v", err)
				}
				if upErr.Status != tc.status {
					t.Fatalf("status = %d, want %d", upErr.Status, tc.status)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%q) = false, want %v", err, tc.want)
			}
		})
	}
}

func TestTruncationOfLargeErrorBody(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 500
	mock.ChatErrorBody = strings.Repeat("x", 2000)

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("want UpstreamError, got %v", err)
	}
	if len(upErr.Body) > 503 {
		t.Errorf("body not truncated: %d chars", len(upErr.Body))
	}
	if !strings.HasSuffix(upErr.Body, "...") {
		t.Errorf("truncation marker missing: %q", upErr.Body)
	}
}

func TestWaitingRoomRetryAfterHeader(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"waiting_room_queued"}`)
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var wrErr *WaitingRoomError
	if !errors.As(err, &wrErr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wrErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", wrErr.RetryAfter)
	}
	if !errors.Is(err, ErrWaitingRoom) {
		t.Error("not unwrap-able to ErrWaitingRoom")
	}
}

func TestSessionControlCalls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.InstanceID != "inst-abc-123" {
		t.Fatalf("create state = %+v", st)
	}
	if st.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}

	// poll requires instance header
	polled, err := client.GetSession(context.Background(), "inst-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "active" {
		t.Errorf("poll status = %q", polled.Status)
	}

	// end + tolerated 404
	if err := client.EndSession(context.Background(), "inst-abc-123"); err != nil {
		t.Fatal(err)
	}
}

// TestProbeAccount verifies the zero-cost token probe: a GET
// /api/v1/freebuff/session with NO instance header that claims no session
// slot, returns the live per-model quota, and classifies
// auth/ban/region/transport failures through the standard matrix.
func TestProbeAccount(t *testing.T) {
	t.Run("200 with quota", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.ProbeAccount(context.Background())
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if st.Status != "active" || st.InstanceID != "inst-abc-123" {
			t.Fatalf("probe state = %+v", st)
		}
		q, ok := st.RateLimitsByModel["deepseek/deepseek-v4-flash"]
		if !ok {
			t.Fatalf("RateLimitsByModel missing flash quota: %+v", st.RateLimitsByModel)
		}
		if q.Limit != 6 || q.RecentCount != 2 {
			t.Errorf("quota limit/recentCount = %v/%v, want 6/2", q.Limit, q.RecentCount)
		}
		if q.Period != "pacific_day" {
			t.Errorf("period = %q, want pacific_day", q.Period)
		}
		if q.ResetAt.IsZero() {
			t.Error("resetAt not parsed")
		}
		// A probe must not claim a session slot (no POST).
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("session creates = %d, want 0 (probe is zero-cost)", got)
		}
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("session probes = %d, want 1", got)
		}
	})

	t.Run("404 maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("200 ended maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("401 auth rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.AuthReject = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want ErrAuthRejected", err)
		}
	})

	t.Run("403 banned", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.Ban = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrBanned) {
			t.Fatalf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("403 country blocked", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrCountryBlocked) {
			t.Fatalf("err = %v, want ErrCountryBlocked", err)
		}
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %T, want *CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" {
			t.Errorf("countryCode = %q, want CN", cbe.CountryCode)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		mock := testutil.NewMock()
		url := mock.URL()
		mock.Close()

		client, _ := New("tok", testConfig(url, nil))
		_, err := client.ProbeAccount(context.Background())
		if err == nil {
			t.Fatal("ProbeAccount returned nil error for closed server")
		}
	})
}

// TestSessionCallParsesRateLimitsByModel verifies the live per-model quota
// map from an admission response is parsed into SessionState, including the
// nested entitlement breakdown and flex-time resetAt.
func TestSessionCallParsesRateLimitsByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q, ok := st.RateLimitsByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("RateLimitsByModel missing model z-ai/glm-5.2: %+v", st.RateLimitsByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not parsed")
	} else if want := "2026-08-16T07:00:00Z"; q.ResetAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("resetAt = %s, want %s", q.ResetAt.UTC().Format(time.RFC3339), want)
	}
	if q.Entitlement["base"] != 1 || q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want base=1 referral=1 streak=3", q.Entitlement)
	}
	if q.Model != "z-ai/glm-5.2" {
		t.Errorf("quota model = %q", q.Model)
	}
}

// TestSessionCallParsesLimitedModelOffers verifies the limited-tier per-model
// allowances from an admission response are parsed into SessionState,
// including flex-time userResetAt.
func TestSessionCallParsesLimitedModelOffers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","accessTier":"limited","limitedModelOffers":[{"model":"deepseek/deepseek-v4-flash","remaining":3,"total":5,"userRemaining":3,"userResetAt":"2026-08-16T07:00:00.000Z"}]}`)
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.LimitedModelOffers) != 1 {
		t.Fatalf("LimitedModelOffers len = %d, want 1: %+v", len(st.LimitedModelOffers), st.LimitedModelOffers)
	}
	offer := st.LimitedModelOffers[0]
	if offer.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q", offer.Model)
	}
	if offer.Remaining != 3 || offer.Total != 5 || offer.UserRemaining != 3 {
		t.Errorf("offer = %+v, want remaining=3 total=5 userRemaining=3", offer)
	}
	wantReset := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	if !offer.UserResetAt.Equal(wantReset) {
		t.Errorf("UserResetAt = %v, want %v", offer.UserResetAt, wantReset)
	}
}

// TestSessionCallIgnoresMissingLimitedModelOffers verifies a full-tier or
// compact admission without limitedModelOffers parses cleanly (nil slice).
func TestSessionCallIgnoresMissingLimitedModelOffers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.LimitedModelOffers != nil {
		t.Errorf("LimitedModelOffers = %+v, want nil when absent", st.LimitedModelOffers)
	}
}

func TestSession404Mapping(t *testing.T) {
	// A create 404 means no session slot exists upstream → disabled.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "404"

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "disabled" {
		t.Errorf("create 404 status = %q, want disabled", st.Status)
	}

	// A poll 404 means the session vanished upstream (expired/evicted) →
	// ended (recreate path), NOT a permanent disabled (which the session
	// manager would cache with no expiry, disabling the token forever).
	polled, err := client.GetSession(context.Background(), "inst-gone")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "ended" {
		t.Errorf("poll 404 status = %q, want ended", polled.Status)
	}
}

func TestQueuedSessionParsing(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.QueuePosition = 4
	mock.QueueDepth = 9
	mock.EstimatedWaitMs = 0

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "queued" || st.Position != 4 || st.QueueDepth != 9 {
		t.Fatalf("queued state = %+v", st)
	}
	if st.PollAt.IsZero() {
		t.Error("pollAt not parsed")
	}
}

func TestStartAndFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	runID, err := client.StartRun(context.Background(), "base2-free-deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-0001" {
		t.Errorf("runID = %q", runID)
	}
	if len(mock.StartedRuns) != 1 || mock.StartedRuns[0] != "base2-free-deepseek-flash" {
		t.Errorf("START not recorded: %v", mock.StartedRuns)
	}

	if err := client.FinishRun(context.Background(), runID, 4); err != nil {
		t.Fatal(err)
	}
	if len(mock.FinishedRuns) != 1 {
		t.Fatalf("FINISH not recorded: %v", mock.FinishedRuns)
	}
	f := mock.FinishedRuns[0]
	if f.RunID != runID || f.Status != "completed" || f.TotalSteps != 4 {
		t.Errorf("FINISH payload = %+v", f)
	}
}

func TestControlCallTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Hang the session create; the 50ms control timeout must win.
	mock.SessionCreateDelay = 2 * time.Second

	client, _ := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.SessionCallTimeout = 50 * time.Millisecond }))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.CreateSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestProxyWiring(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) { c.HTTPProxy = "http://127.0.0.1:9999" })
	client, err := New("tok", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.http.Transport.(*http.Transport).Proxy == nil {
		t.Error("HTTP proxy not wired")
	}

	socksCfg := testConfig("", func(c *config.Config) { c.SOCKS5Proxy = "socks5://127.0.0.1:1080" })
	socksClient, err := New("tok", socksCfg)
	if err != nil {
		t.Fatal(err)
	}
	if socksClient.http.Transport.(*http.Transport).DialContext == nil {
		t.Error("SOCKS5 dialer not wired")
	}
}

// TestSOCKS5RotationDisablesKeepAlives verifies round-robin/random rotation
// is not defeated by pooled idle connections: with multiple SOCKS5 proxies
// the transport must redial per request (DisableKeepAlives) so the
// per-request proxy choice is actually dialed, while the single-proxy path
// keeps pooled connections.
func TestSOCKS5RotationDisablesKeepAlives(t *testing.T) {
	multi, err := New("tok", testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
		c.ProxyRotation = "round-robin"
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !multi.http.Transport.(*http.Transport).DisableKeepAlives {
		t.Error("multi-proxy rotation must disable keep-alives so every request dials through its assigned proxy")
	}

	single, err := New("tok", testConfig("", func(c *config.Config) { c.SOCKS5Proxy = "socks5://127.0.0.1:1080" }))
	if err != nil {
		t.Fatal(err)
	}
	if single.http.Transport.(*http.Transport).DisableKeepAlives {
		t.Error("single SOCKS5 proxy must keep pooled keep-alive connections")
	}
}

// TestSOCKS5IgnoresEnvProxy verifies the SOCKS5 branches drop the
// ProxyFromEnvironment inherited from http.DefaultTransport.Clone: an
// operator HTTP_PROXY/HTTPS_PROXY env var must never double-route SOCKS5
// traffic through a second proxy.
func TestSOCKS5IgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9998")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9998")

	multi, err := New("tok", testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if tr := multi.http.Transport.(*http.Transport); tr.Proxy != nil {
		t.Error("SOCKS5_PROXIES transport still routes via ProxyFromEnvironment")
	}

	single, err := New("tok", testConfig("", func(c *config.Config) { c.SOCKS5Proxy = "socks5://127.0.0.1:1080" }))
	if err != nil {
		t.Fatal(err)
	}
	if tr := single.http.Transport.(*http.Transport); tr.Proxy != nil {
		t.Error("SOCKS5_PROXY transport still routes via ProxyFromEnvironment")
	}
}

// TestHTTPProxyStealthUsesConnectTunnel verifies HTTP_PROXY + TLS_FINGERPRINT
// routes the stealth dialer through an explicit CONNECT tunnel instead of
// transport.Proxy: Go calls DialTLSContext with the proxy's address for
// proxied HTTPS (not the origin), so transport.Proxy would hand the stealth
// ClientHello to the plain CONNECT proxy and break the tunnel.
func TestHTTPProxyStealthUsesConnectTunnel(t *testing.T) {
	stealthClient, err := New("tok", testConfig("", func(c *config.Config) {
		c.HTTPProxy = "http://127.0.0.1:9999"
		c.TLSFingerprint = "chrome126"
	}))
	if err != nil {
		t.Fatal(err)
	}
	tr := stealthClient.http.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("HTTP_PROXY + TLS_FINGERPRINT must not route via transport.Proxy (Go would TLS to the proxy, not the origin)")
	}
	if tr.DialTLSContext == nil {
		t.Error("HTTP_PROXY + TLS_FINGERPRINT must wire the stealth DialTLSContext over the CONNECT tunnel")
	}

	plainClient, err := New("tok", testConfig("", func(c *config.Config) { c.HTTPProxy = "http://127.0.0.1:9999" }))
	if err != nil {
		t.Fatal(err)
	}
	plainTr := plainClient.http.Transport.(*http.Transport)
	if plainTr.Proxy == nil {
		t.Error("HTTP_PROXY without TLS_FINGERPRINT should keep transport.Proxy routing")
	}
	if plainTr.DialTLSContext != nil {
		t.Error("HTTP_PROXY without TLS_FINGERPRINT must not wire DialTLSContext")
	}
}

// TestHTTPConnectDial exercises the CONNECT tunnel against a real proxy
// listener: the CONNECT request line carries the target, Proxy-Authorization
// is sent when the proxy URL has credentials, bytes flow both ways through
// the tunnel, and a non-200 CONNECT response is rejected.
func TestHTTPConnectDial(t *testing.T) {
	t.Run("tunnel", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()

		type proxyObs struct {
			reqLine string
			echo    string
		}
		obs := make(chan proxyObs, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			reqLine, err := br.ReadString('\n')
			if err != nil {
				return
			}
			for {
				line, err := br.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
			buf := make([]byte, 4)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			_, _ = io.WriteString(conn, "pong")
			obs <- proxyObs{reqLine: reqLine, echo: string(buf)}
		}()

		dial := httpConnectDial(&url.URL{Scheme: "http", Host: ln.Addr().String()})
		conn, err := dial(context.Background(), "tcp", "origin.example:443")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 4)
		if _, err := io.ReadFull(conn, reply); err != nil {
			t.Fatal(err)
		}
		if string(reply) != "pong" {
			t.Errorf("tunnel reply = %q, want pong", reply)
		}
		got := <-obs
		if !strings.Contains(got.reqLine, "CONNECT origin.example:443 HTTP/1.1") {
			t.Errorf("CONNECT request line = %q, want target origin.example:443", got.reqLine)
		}
		if got.echo != "ping" {
			t.Errorf("tunnel carried %q, want ping", got.echo)
		}
	})

	t.Run("proxy auth", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()

		authCh := make(chan string, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			var auth string
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.HasPrefix(strings.ToLower(line), "proxy-authorization:") {
					auth = strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
				}
				if line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
			authCh <- auth
			_, _ = io.Copy(io.Discard, br)
		}()

		dial := httpConnectDial(&url.URL{Scheme: "http", User: url.UserPassword("alice", "s3cret"), Host: ln.Addr().String()})
		conn, err := dial(context.Background(), "tcp", "origin.example:443")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
		if got := <-authCh; !strings.EqualFold(got, want) {
			t.Errorf("Proxy-Authorization = %q, want %q", got, want)
		}
	})

	t.Run("non-200 rejected", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			for {
				line, err := br.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		}()

		dial := httpConnectDial(&url.URL{Scheme: "http", Host: ln.Addr().String()})
		conn, err := dial(context.Background(), "tcp", "origin.example:443")
		if err == nil {
			_ = conn.Close()
			t.Fatal("CONNECT through a 403 proxy succeeded, want error")
		}
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error = %q, want proxy 403 status", err)
		}
	})

	t.Run("silent proxy times out", func(t *testing.T) {
		// A proxy that accepts TCP but never answers CONNECT must not leak a
		// dial goroutine and conn forever: the exchange is deadline-bounded.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the conn open, never write a response.
			_, _ = io.Copy(io.Discard, conn)
		}()

		old := httpConnectTimeout
		httpConnectTimeout = 100 * time.Millisecond
		defer func() { httpConnectTimeout = old }()

		dial := httpConnectDial(&url.URL{Scheme: "http", Host: ln.Addr().String()})
		start := time.Now()
		conn, err := dial(context.Background(), "tcp", "origin.example:443")
		if err == nil {
			_ = conn.Close()
			t.Fatal("CONNECT to a silent proxy succeeded, want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("CONNECT timeout took %v, want bounded by the 100ms deadline", elapsed)
		}
		if !strings.Contains(err.Error(), "CONNECT origin.example:443") {
			t.Errorf("timeout error = %q, want the target address", err)
		}
	})
}

// TestCrossHostRedirectStripsToken verifies a cross-host redirect does not
// carry x-codebuff-api-key (or Authorization): Go strips the latter itself
// but not the former, so the raw token used to leak to any redirect target.
// Same-host redirects keep their credentials (CDN / bare-host -> www).
func TestCrossHostRedirectStripsToken(t *testing.T) {
	const token = "tok-secret-redirect"

	t.Run("scheme downgrade strips token", func(t *testing.T) {
		// https -> http to the SAME host (plaintext) must drop both
		// credential headers; the token must never cross onto a cleartext
		// hop, even when the hostname is unchanged.
		check := func(t *testing.T, from, to string, wantStripped bool) {
			t.Helper()
			client, err := New(token, testConfig("http://127.0.0.1:1", nil))
			if err != nil {
				t.Fatal(err)
			}
			via := []*http.Request{{
				URL:    mustParseURL(t, from),
				Header: http.Header{},
			}}
			via[0].Header.Set("x-codebuff-api-key", token)
			via[0].Header.Set("Authorization", "Bearer "+token)
			req := &http.Request{URL: mustParseURL(t, to), Header: via[0].Header.Clone()}
			if err := client.http.CheckRedirect(req, via); err != nil {
				t.Fatalf("CheckRedirect: %v", err)
			}
			gotKey := req.Header.Get("x-codebuff-api-key")
			gotAuth := req.Header.Get("Authorization")
			if wantStripped {
				if gotKey != "" || gotAuth != "" {
					t.Errorf("redirect %s -> %s kept credentials (key %q auth %q), want stripped", from, to, gotKey, gotAuth)
				}
			} else {
				if gotKey != token || gotAuth != "Bearer "+token {
					t.Errorf("redirect %s -> %s stripped credentials (key %q auth %q), want kept", from, to, gotKey, gotAuth)
				}
			}
		}
		check(t, "https://www.codebuff.com", "http://www.codebuff.com", true)
		check(t, "https://www.codebuff.com", "http://www.codebuff.com:8080", true)
		check(t, "https://www.codebuff.com", "https://www.codebuff.com", false)
		check(t, "http://www.codebuff.com", "https://www.codebuff.com", false)
	})

	keySeen := make(chan string, 1)
	authSeen := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keySeen <- r.Header.Get("x-codebuff-api-key")
		authSeen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/final", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(token, testConfig(origin.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-keySeen; got != "" {
		t.Errorf("cross-host redirect carried x-codebuff-api-key %q, want stripped", got)
	}
	if got := <-authSeen; got != "" {
		t.Errorf("cross-host redirect carried Authorization %q, want stripped", got)
	}

	sameKey := make(chan string, 1)
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		sameKey <- r.Header.Get("x-codebuff-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer same.Close()

	sameClient, err := New(token, testConfig(same.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	sameReq, err := sameClient.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	sameResp, err := sameClient.http.Do(sameReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = sameResp.Body.Close()
	if got := <-sameKey; got != token {
		t.Errorf("same-host redirect carried x-codebuff-api-key %q, want %q kept", got, token)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestClientIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := generateClientID()
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
			t.Fatalf("client_id %q not 13-char base36", id)
		}
	}
}

// TestGenerateClientIDFallbackPads verifies the time-seeded fallback never
// panics on a short base36 value: UnixNano in base36 is 12 digits today, and
// the old [:13] slice on it panicked whenever crypto/rand failed. The shared
// padBase36 helper must always yield the SDK's 13-char id.
func TestGenerateClientIDFallbackPads(t *testing.T) {
	for i := 0; i < 10; i++ {
		fallback := padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(fallback) {
			t.Fatalf("time fallback client_id %q not 13-char base36", fallback)
		}
	}
	if got := padBase36("abc"); got != "0000000000abc" {
		t.Errorf("padBase36(abc) = %q, want 0000000000abc (13 chars)", got)
	}
	if got := padBase36("0123456789abc"); got != "0123456789abc" {
		t.Errorf("padBase36(13-char) = %q, want unchanged", got)
	}
}

func TestNewTLSFingerprintInvalid(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) { c.TLSFingerprint = "bogus" })
	_, err := New("tok", cfg)
	if err == nil {
		t.Fatal("New with bogus TLS_FINGERPRINT succeeded, want error")
	}
	if !strings.Contains(err.Error(), "TLS_FINGERPRINT") {
		t.Errorf("error = %q, want mention of TLS_FINGERPRINT", err)
	}
}

func TestAbortPropagation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBlocks = true

	client, _ := New("tok", testConfig(mock.URL(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err == nil {
			_ = rc.Close()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatCompletions error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions still blocked after cancel")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !mock.AbortDetected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upstream request was not aborted on client cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClassifyRateLimit(t *testing.T) {
	body := `{"model":"deepseek/deepseek-v4-flash","entitlementBreakdown":{"base":6},"limit":6,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","windowHours":24,"recentCount":6.6,"status":"rate_limited","accessTier":"limited","retryAfterMs":48549499}`
	err := classifyError(429, body, http.Header{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("errors.Is(ErrRateLimited) = false, got %v", err)
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 48549499*time.Millisecond {
		t.Errorf("RetryAfter = %s, want 48549499ms", rle.RetryAfter)
	}
	if rle.Limit != 6 {
		t.Errorf("Limit = %v, want 6", rle.Limit)
	}
	if rle.RecentCount != 6.6 {
		t.Errorf("RecentCount = %v, want 6.6", rle.RecentCount)
	}
	wantReset, _ := time.Parse(time.RFC3339Nano, "2026-08-12T07:00:00.000Z")
	if !rle.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v, want %v", rle.ResetAt, wantReset)
	}

	// Nested error payload with snake_case fields
	bodyNested := `{"error":{"status":"rate_limited","reset_at":"2026-08-15T07:00:00Z","retry_after_ms":120000}}`
	errNested := classifyError(429, bodyNested, http.Header{})
	if !errors.Is(errNested, ErrRateLimited) {
		t.Fatalf("nested: errors.Is(ErrRateLimited) = false, got %v", errNested)
	}
	var rleNested *RateLimitError
	if !errors.As(errNested, &rleNested) {
		t.Fatalf("nested: want *RateLimitError, got %v", errNested)
	}
	if rleNested.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %s, want 120s", rleNested.RetryAfter)
	}

	// Generic 429 without explicit timestamp auto-detects upcoming Pacific midnight
	errGeneric := classifyError(429, `{"status":"rate_limited"}`, http.Header{})
	var rleGeneric *RateLimitError
	if errors.As(errGeneric, &rleGeneric) {
		if rleGeneric.ResetAt.IsZero() {
			t.Errorf("expected auto-detected ResetAt, got zero")
		}
		if !rleGeneric.ResetAt.After(time.Now()) {
			t.Errorf("expected ResetAt to be in the future, got %v", rleGeneric.ResetAt)
		}
	}

	// Header fallback when body has no JSON quota fields.
	err2 := classifyError(429, "opaque body", http.Header{"Retry-After": {"300"}})
	if !errors.Is(err2, ErrRateLimited) {
		t.Fatalf("header fallback: errors.Is(ErrRateLimited) = false, got %v", err2)
	}
	var rle2 *RateLimitError
	if !errors.As(err2, &rle2) {
		t.Fatalf("header fallback: want *RateLimitError, got %v", err2)
	}
	if rle2.RetryAfter != 300*time.Second {
		t.Errorf("RetryAfter = %s, want 300s (header fallback)", rle2.RetryAfter)
	}
}

func TestNextPacificMidnight(t *testing.T) {
	next := NextPacificMidnight()
	if !next.After(time.Now()) {
		t.Fatalf("NextPacificMidnight %v is not after now %v", next, time.Now())
	}
	if next.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", next.Location())
	}
}

func TestWrapDecompress(t *testing.T) {
	const want = `{"status":"active","instanceId":"inst-abc-123"}`
	cases := []struct {
		name       string
		encoding   string
		compress   func([]byte) []byte
		wantErrSub string
	}{
		{"identity passthrough", "", nil, ""},
		{"gzip", "gzip", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"deflate", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		// RFC 9110 §8.4.1.3: deflate = zlib-wrapped (RFC 1950). A conforming
		// server's body must decode; the raw-flate fallback must not break
		// the existing raw case above. (Audit B1.)
		{"deflate zlib-wrapped", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"corrupt gzip", "gzip", func(b []byte) []byte {
			return []byte("this is not gzip data")
		}, "gzip:"},
		// Multi-value Content-Encoding is rejected with a clear error, not
		// silently mis-decoded. (Audit G1.)
		{"multi-value encoding rejected", "gzip, br", nil, "unsupported Content-Encoding"},
		{"brotli", "br", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := brotli.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"zstd", "zstd", func(b []byte) []byte {
			var buf bytes.Buffer
			zw, _ := zstd.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"unsupported encoding", "lz4", nil, "unsupported Content-Encoding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(want)
			if tc.compress != nil {
				body = tc.compress([]byte(want))
			}
			resp := &http.Response{
				Header: http.Header{},
				Body:   io.NopCloser(bytes.NewReader(body)),
			}
			if tc.encoding != "" {
				resp.Header.Set("Content-Encoding", tc.encoding)
			}
			err := wrapDecompress(resp)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("wrapDecompress err = %v, want %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("wrapDecompress: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_ = resp.Body.Close()
			if string(got) != want {
				t.Errorf("body = %q, want %q", got, want)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding header not stripped")
			}
		})
	}
}

func TestClassifyBan(t *testing.T) {
	resumesAt := "2026-07-21T09:18:07+00:00"
	body := `{"status":"banned","resumes_at":"` + resumesAt + `"}`
	err := classifyError(403, body, http.Header{})
	if !errors.Is(err, ErrBanned) {
		t.Fatalf("errors.Is(ErrBanned) = false, got %v", err)
	}
	var be *BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *BanError, got %v", err)
	}
	wantTime, _ := time.Parse(time.RFC3339, resumesAt)
	if !be.ResumesAt.Equal(wantTime) {
		t.Errorf("ResumesAt = %v, want %v", be.ResumesAt, wantTime)
	}

	// 403 banned without resumes_at.
	bodyNoTime := `{"status":"banned"}`
	err2 := classifyError(403, bodyNoTime, http.Header{})
	if !errors.Is(err2, ErrBanned) {
		t.Fatalf("errors.Is(ErrBanned) = false for no-resumes_at, got %v", err2)
	}
	var be2 *BanError
	if !errors.As(err2, &be2) {
		t.Fatalf("want *BanError, got %v", err2)
	}
	if !be2.ResumesAt.IsZero() {
		t.Errorf("ResumesAt = %v, want zero for missing resumes_at", be2.ResumesAt)
	}

	// 403 WITHOUT "status":"banned" must NOT be ErrBanned.
	bodyOther := `{"error":"forbidden"}`
	err3 := classifyError(403, bodyOther, http.Header{})
	if errors.Is(err3, ErrBanned) {
		t.Fatalf("403 without banned status must not be ErrBanned, got %v", err3)
	}
	var ue *UpstreamError
	if !errors.As(err3, &ue) {
		t.Fatalf("want UpstreamError, got %v", err3)
	}
}

// TestClassifyBanUnixMsResumesAt verifies parseBan decodes a unix-ms
// resumes_at (not just RFC3339): flex-time parsing must recover the unban
// time so the cooldown ends when the ban actually lifts.
func TestClassifyBanUnixMsResumesAt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Time
	}{
		{"unix milliseconds", `{"status":"banned","resumes_at":1753075087000}`, time.UnixMilli(1753075087000)},
		{"unix seconds", `{"status":"banned","resumes_at":1753075087}`, time.Unix(1753075087, 0)},
		{"rfc3339", `{"status":"banned","resumes_at":"2026-07-21T09:18:07+00:00"}`, time.Date(2026, 7, 21, 9, 18, 7, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(403, tc.body, http.Header{})
			var be *BanError
			if !errors.As(err, &be) {
				t.Fatalf("want *BanError, got %v", err)
			}
			if !be.ResumesAt.Equal(tc.want) {
				t.Errorf("ResumesAt = %v, want %v", be.ResumesAt, tc.want)
			}
		})
	}
}

// TestClassifyCredits verifies a 402 payment-required response maps to a
// CreditsError unwrapping to ErrCredits (fresh free accounts hit this before
// the free tier kicks in, so it must NOT fall through to a generic
// UpstreamError).
func TestClassifyCredits(t *testing.T) {
	err := classifyError(402, `{"error":"insufficient credits"}`, http.Header{})
	var credErr *CreditsError
	if !errors.As(err, &credErr) {
		t.Fatalf("want CreditsError, got %v", err)
	}
	if credErr.Status != 402 {
		t.Errorf("status = %d, want 402", credErr.Status)
	}
	if !errors.Is(err, ErrCredits) {
		t.Error("not unwrap-able to ErrCredits")
	}
}

// TestClassifyFreeModeCLIRequired verifies the free-tier gate refusal is
// typed, so the gateway can distinguish "envelope missing" from a hard 403.
func TestClassifyFreeModeCLIRequired(t *testing.T) {
	body := `{"error":{"status":"free_mode_cli_required","message":"CLI fingerprint required for free tier"}}`
	err := classifyError(403, body, http.Header{})
	if !errors.Is(err, ErrFreeModeCLIRequired) {
		t.Fatalf("errors.Is(ErrFreeModeCLIRequired) = false, got %v", err)
	}
}

// TestClassifyCountryBlocked verifies a 403 country_blocked response maps to
// a CountryBlockedError carrying the parsed region fields.
func TestClassifyCountryBlocked(t *testing.T) {
	body := `{"status":"country_blocked","countryCode":"US","countryBlockReason":"Free mode is not available in your country","ipPrivacySignals":["vpn","proxy"]}`
	err := classifyError(403, body, http.Header{})
	var cbe *CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "US" {
		t.Errorf("countryCode = %q, want US", cbe.CountryCode)
	}
	if cbe.CountryBlockReason != "Free mode is not available in your country" {
		t.Errorf("countryBlockReason = %q", cbe.CountryBlockReason)
	}
	if len(cbe.IpPrivacySignals) != 2 || cbe.IpPrivacySignals[0] != "vpn" || cbe.IpPrivacySignals[1] != "proxy" {
		t.Errorf("ipPrivacySignals = %v", cbe.IpPrivacySignals)
	}
	if !errors.Is(err, ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

// TestClassifyCountryBlockedToleratesAbsentFields verifies a bare
// country_blocked body (compact poll) still classifies without panicking and
// leaves the optional fields zero.
func TestClassifyCountryBlockedToleratesAbsentFields(t *testing.T) {
	err := classifyError(403, `{"status":"country_blocked"}`, http.Header{})
	var cbe *CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "" || cbe.CountryBlockReason != "" || len(cbe.IpPrivacySignals) != 0 {
		t.Errorf("expected zero optional fields, got %+v", cbe)
	}
	if !errors.Is(err, ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

// TestClassifyDeploymentOutsideHoursRetryable verifies a
// deployment_outside_hours body (when no other classifier claims it) maps to
// an UpstreamError marked Retryable, not a hard failure.
func TestClassifyDeploymentOutsideHoursRetryable(t *testing.T) {
	err := classifyError(500, `{"status":"deployment_outside_hours","message":"Free mode is only available during operating hours"}`, http.Header{})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("want UpstreamError, got %v", err)
	}
	if !upErr.Retryable {
		t.Error("Retryable = false, want true")
	}
	if upErr.Status != 500 {
		t.Errorf("status = %d, want 500", upErr.Status)
	}

	// Ordinary 500s stay non-retryable.
	errPlain := classifyError(500, `{"error":"boom"}`, http.Header{})
	var plain *UpstreamError
	if !errors.As(errPlain, &plain) {
		t.Fatalf("want UpstreamError, got %v", errPlain)
	}
	if plain.Retryable {
		t.Error("plain UpstreamError must not be Retryable")
	}
}

// TestStealthProfileResolvedOncePerRequest verifies that for TLS_FINGERPRINT
// auto/random the concrete profile is resolved ONCE per request: newRequest
// stashes it (and applies its headers), and the dialer reads the same stash
// for the ClientHello — so headers and TLS fingerprint never mismatch.
func TestStealthProfileResolvedOncePerRequest(t *testing.T) {
	client, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "auto" }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	stashed := stealthProfileFrom(req.Context())
	if stashed == nil {
		t.Fatal("no concrete profile stashed in the request context")
	}
	if stashed.ID == stealth.ProfileIDAuto || stashed.ID == stealth.ProfileIDRandom {
		t.Fatalf("stashed profile %s is not concrete (auto must resolve once)", stashed.ID)
	}
	// The browser headers were applied from the SAME concrete profile.
	if got := req.Header.Get("User-Agent"); got != stashed.UserAgent {
		t.Errorf("request User-Agent %q != stashed profile User-Agent %q", got, stashed.UserAgent)
	}
	// The dialer must use the stashed profile for this request's dial.
	if dial := client.dialProfileFor(req.Context()); dial != stashed {
		t.Errorf("dialProfileFor(request ctx) = %p (%s), want the stashed profile %p", dial, dial.ID, stashed)
	}
	// A bare context (no stash) falls back to the unresolved profile; the
	// dialer resolves it per connection (pre-fix behavior for dials that
	// never went through newRequest).
	if dial := client.dialProfileFor(context.Background()); dial != stealth.ProfileAuto {
		t.Errorf("dialProfileFor(bare ctx) = %v, want ProfileAuto (dialer resolves per connection)", dial)
	}
	// Pinned profiles keep working unchanged.
	pinned, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "chrome126" }))
	if err != nil {
		t.Fatal(err)
	}
	if dial := pinned.dialProfileFor(context.Background()); dial != stealth.ProfileChrome126 {
		t.Errorf("pinned dialProfileFor = %s, want chrome126", dial.ID)
	}
}

// TestTransientRetriesNotCountedWhenRetryCannotFire verifies the transient
// retry counter only counts retries that actually fire: no GetBody (GET) and
// a failed body replay must both leave the counter at 0.
func TestTransientRetriesNotCountedWhenRetryCannotFire(t *testing.T) {
	t.Run("nil GetBody never counts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 1
		rt.err = errors.New("tls handshake failed")

		req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		if req.GetBody != nil {
			t.Fatal("GET request should have nil GetBody")
		}
		req.Body = http.NoBody // GETs carry no body; the transport needs a non-nil reader
		resp, cancel, err := client.do(req, time.Second)
		if err == nil {
			_ = resp.Body.Close()
			releaseCancel(cancel)
			t.Fatal("want error (no retry possible for nil GetBody)")
		}
		if rt.calls.Load() != 1 {
			t.Errorf("upstream attempts = %d, want 1 (no retry for nil GetBody)", rt.calls.Load())
		}
		if got := client.TransientRetries(); got != 0 {
			t.Errorf("TransientRetries = %d, want 0", got)
		}
	})

	t.Run("failed body replay never counts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 1
		rt.err = errors.New("tls handshake failed")

		req, err := client.newRequest(context.Background(), http.MethodPost, "/api/v1/freebuff/session", []byte("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("replay unavailable") }
		resp, cancel, err := client.do(req, time.Second)
		if err == nil {
			_ = resp.Body.Close()
			releaseCancel(cancel)
			t.Fatal("want error when replay fails and no retry fires")
		}
		if got := client.TransientRetries(); got != 0 {
			t.Errorf("TransientRetries = %d, want 0 (counted only after successful replay)", got)
		}
	})
}

// TestPacificMidnightFallback pins the tzdata-less fallback: Pacific is
// UTC-7 (07:00 UTC midnight) March-November and UTC-8 (08:00 UTC) otherwise.
func TestPacificMidnightFallback(t *testing.T) {
	jan := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if got := pacificMidnightFallback(jan); got.Hour() != 8 {
		t.Errorf("January fallback hour = %d, want 8 (PST)", got.Hour())
	}
	jul := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	if got := pacificMidnightFallback(jul); got.Hour() != 7 {
		t.Errorf("July fallback hour = %d, want 7 (PDT)", got.Hour())
	}
	if !pacificMidnightFallback(jan).After(jan) || !pacificMidnightFallback(jul).After(jul) {
		t.Error("fallback must return a time after the reference now")
	}
}

func TestProxyRotationRoundRobin(t *testing.T) {
	client, err := New("tok-a", testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
		c.ProxyRotation = "round-robin"
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.socksProxies) != 2 || len(client.socksDialers) != 2 {
		t.Fatalf("proxies = %v, dialers = %d, want 2 each", client.socksProxies, len(client.socksDialers))
	}

	got := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		idx, ok := req.Context().Value(proxyIndexKey{}).(int)
		if !ok {
			t.Fatal("proxy index not stashed in request context")
		}
		got = append(got, idx)
	}
	want := []int{0, 1, 0, 1, 0}
	if len(got) != len(want) {
		t.Fatalf("rotation sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rotation sequence = %v, want %v (consecutive requests must alternate)", got, want)
			break
		}
	}
}

func TestProxyRotationRandom(t *testing.T) {
	client, err := New("tok-a", testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
		c.ProxyRotation = "random"
	}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for i := 0; i < 40; i++ {
		req, err := client.newRequest(context.Background(), http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		idx, ok := req.Context().Value(proxyIndexKey{}).(int)
		if !ok {
			t.Fatal("proxy index not stashed in request context")
		}
		seen[idx]++
	}
	if len(seen) != 2 {
		t.Errorf("random rotation used %d proxies across 40 requests, want both", len(seen))
	}
}

func TestProxyIndexFor(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002", "socks5://127.0.0.1:1003"}
	})
	// No stash → per-token binding (token tokenIndex → proxy tokenIndex % n).
	c0, err := New("tok", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c0.proxyIndexFor(context.Background()); got != 0 {
		t.Errorf("per-token (index 0) = %d, want 0", got)
	}
	c2, err := NewWithIndex("tok", 2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.proxyIndexFor(context.Background()); got != 2 {
		t.Errorf("per-token (index 2) = %d, want 2", got)
	}
	// A stashed index wins — the dialer honors the per-request choice.
	ctx := context.WithValue(context.Background(), proxyIndexKey{}, 1)
	if got := c0.proxyIndexFor(ctx); got != 1 {
		t.Errorf("proxyIndexFor(stash=1) = %d, want 1", got)
	}
}
func TestCreateSessionForModelHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.CreateSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.Model != "thudm/glm-5.2" || st.InstanceID != "inst-1" {
		t.Errorf("got %+v, want active with model thudm/glm-5.2", st)
	}
}

func TestGetSessionWithOptsHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotCompact, gotHeartbeat, gotInstance string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact = r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat = r.Header.Get("x-freebuff-heartbeat")
		gotInstance = r.Header.Get("x-freebuff-instance-id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Errorf("status = %q, want active", st.Status)
	}
	if gotCompact != "1" || gotHeartbeat != "1" || gotInstance != "inst-1" {
		t.Errorf("headers: compact=%q, heartbeat=%q, instance=%q", gotCompact, gotHeartbeat, gotInstance)
	}
}

func TestSessionCallStructured4xx(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantStatus string
	}{
		{
			name:       "model_locked 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_locked","currentModel":"deepseek/deepseek-v4-flash","requestedModel":"thudm/glm-5.2"}`,
			wantStatus: "model_locked",
		},
		{
			name:       "model_unavailable 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"08:00-20:00"}`,
			wantStatus: "model_unavailable",
		},
		{
			name:       "ip_capped 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`,
			wantStatus: "ip_capped",
		},
		{
			name:       "spend_limited 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`,
			wantStatus: "spend_limited",
		},
		{
			name:       "country_blocked 403",
			statusCode: http.StatusForbidden,
			body:       `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"country_not_allowed"}`,
			wantStatus: "country_blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}

			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}

			st, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", st.Status, tc.wantStatus)
			}
		})
	}
}

// flakyRT is a RoundTripper that fails the first failN calls with a fixed
// transport error, then serves a canned 200 SSE response. It records every
// request body and header so tests can assert GetBody replay and fingerprint
// rotation.
type flakyRT struct {
	failN       int
	calls       atomic.Int32
	err         error
	body        []byte
	seen        [][]byte
	header      http.Header
	seenHeaders []http.Header
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	b, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	f.seen = append(f.seen, b)
	f.seenHeaders = append(f.seenHeaders, req.Header.Clone())
	if int(f.calls.Load()) <= f.failN {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     f.header,
		Body:       io.NopCloser(bytes.NewReader(f.body)),
		Request:    req,
	}, nil
}

// newRetryClient builds a client with TRANSIENT_RETRIES enabled and a pinned
// TLS fingerprint (optional), with the retry backoff pinned to 1ms.
func newRetryClient(t *testing.T, baseURL string, retries int, fingerprint string) (*Client, *flakyRT) {
	t.Helper()
	client, err := New("tok-a", testConfig(baseURL, func(c *config.Config) {
		c.TransientRetries = retries
		c.TLSFingerprint = fingerprint
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }
	rt := &flakyRT{}
	client.http.Transport = rt
	return client, rt
}

// TestDumpRedactsTokenHeaders verifies the debug dump redacts both the
// Authorization header and x-codebuff-api-key (which carries the same token).
// Regression: dump() only redacted Authorization, so DEBUG_DUMP=true leaked
// the plaintext token into dump/ files via x-codebuff-api-key.
func TestDumpRedactsTokenHeaders(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusUnauthorized
	mock.ChatErrorBody = `{"error":"unauthorized"}`

	client, err := New("tok-secret-1234", testConfig(mock.URL(), func(c *config.Config) {
		c.DebugDump = true
	}))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, body); err == nil {
		t.Fatal("expected error from 401 response")
	}

	entries, err := filepath.Glob(filepath.Join("dump", "*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no dump file written")
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	dump := string(data)
	if strings.Contains(dump, "tok-secret-1234") {
		t.Fatalf("dump file leaks token:\n%s", dump)
	}
	if !strings.Contains(dump, "Authorization: [redacted]") {
		t.Errorf("dump file missing redacted Authorization header:\n%s", dump)
	}
	if !strings.Contains(dump, "X-Codebuff-Api-Key: [redacted]") {
		t.Errorf("dump file missing redacted X-Codebuff-Api-Key header:\n%s", dump)
	}
}

func TestChatCompletionsRetriesTransientFailure(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`) + "data: [DONE]\n\n")

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after transient retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if rt.calls.Load() != 2 {
		t.Errorf("upstream attempts = %d, want 2 (1 failure + 1 retry)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	// GetBody replay must re-send an identical payload.
	if len(rt.seen) != 2 || string(rt.seen[0]) != string(rt.seen[1]) {
		t.Errorf("replayed body differs: %q vs %q", rt.seen[0], rt.seen[1])
	}
	if !strings.Contains(string(rt.seen[0]), `"run_id":"r"`) {
		t.Errorf("first attempt body missing envelope: %q", rt.seen[0])
	}
}

func TestChatCompletionsRetriesTwiceWhenAllowed(t *testing.T) {
	client, rt := newRetryClient(t, "", 2, "")
	rt.failN = 2
	rt.err = io.EOF
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`) + "data: [DONE]\n\n")

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after 2 retries: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if rt.calls.Load() != 3 {
		t.Errorf("upstream attempts = %d, want 3 (2 failures + 2 retries)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 2 {
		t.Errorf("TransientRetries = %d, want 2", got)
	}
	for i := 1; i < len(rt.seen); i++ {
		if string(rt.seen[i]) != string(rt.seen[0]) {
			t.Errorf("attempt %d body differs from attempt 0: %q vs %q", i, rt.seen[i], rt.seen[0])
		}
	}
}

func TestCreateSessionRetriesConnectionReset(t *testing.T) {
	// A real abrupt connection close surfaces as context.Canceled on some
	// platforms (Go cancels the request context when the server tears the
	// connection down mid-request), which MUST NOT be retried. Inject the
	// transport-level reset at the RoundTripper boundary instead: this is
	// the same code path a live dial/TLS failure takes.
	rt := &flakyRT{
		failN:  1,
		err:    errors.New("read tcp 127.0.0.1:443: connection reset by peer"),
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   []byte(`{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`),
	}
	client, err := New("tok-a", testConfig("", func(c *config.Config) { c.TransientRetries = 1 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }
	client.SetTransport(rt)

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession failed after retry: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("session = %+v, want active inst-1", st)
	}
	if rt.calls.Load() != 2 {
		t.Errorf("upstream attempts = %d, want 2 (1 failure + 1 retry)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	// The session POST body was replayed identically.
	if len(rt.seen) != 2 || string(rt.seen[0]) != string(rt.seen[1]) {
		t.Errorf("replayed session body differs: %q vs %q", rt.seen[0], rt.seen[1])
	}
}

func TestRateLimitNeverRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true

	client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (429 must never be retried)", mock.Requests)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestBanNeverRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if !errors.Is(err, ErrBanned) {
		t.Fatalf("err = %v, want ErrBanned", err)
	}
	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (403 banned must never be retried)", mock.Requests)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestTransientRetriesDisabledSingleAttempt(t *testing.T) {
	client, rt := newRetryClient(t, "", 0, "")
	rt.failN = 100
	rt.err = errors.New("connection reset by peer")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err == nil {
		t.Fatal("want error when every attempt fails")
	}
	if rt.calls.Load() != 1 {
		t.Errorf("upstream attempts = %d, want exactly 1 (TRANSIENT_RETRIES=0)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestRetryRotatesPinnedFingerprint(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "chrome126")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1", got)
	}
	client.profileMu.Lock()
	id := client.stealthProfile.ID
	client.profileMu.Unlock()
	if id != stealth.ProfileIDSafari18 {
		t.Errorf("stealthProfile = %s, want safari18 (chrome126 rotated to a distinct JA3)", id)
	}
	// The retried request carried the rotated profile's browser headers:
	// the first attempt used chrome126, the retry re-applied safari18.
	if rt.calls.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want 2", rt.calls.Load())
	}
	if got := rt.seenHeaders[0].Get("User-Agent"); got != stealth.ProfileChrome126.UserAgent {
		t.Errorf("attempt 1 User-Agent = %q, want chrome126", got)
	}
	if got := rt.seenHeaders[1].Get("User-Agent"); got != stealth.ProfileSafari18.UserAgent {
		t.Errorf("attempt 2 User-Agent = %q, want safari18 (rotated)", got)
	}
	// The retry rotated to Safari18, which has no Client Hints: the Chromium
	// Sec-CH-UA* headers from attempt 1 must be GONE, not carried onto a
	// Safari TLS fingerprint.
	if got := rt.seenHeaders[0].Get("Sec-CH-UA"); got == "" {
		t.Error("attempt 1 Sec-CH-UA empty, want chrome126 client hints")
	}
	for _, hdr := range []string{"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
		if got := rt.seenHeaders[1].Get(hdr); got != "" {
			t.Errorf("attempt 2 %s = %q, want deleted (Safari18 has no Client Hints)", hdr, got)
		}
	}
}

func TestRetryDoesNotRotateAutoProfile(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "auto")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if got := client.FingerprintRotations(); got != 0 {
		t.Errorf("FingerprintRotations = %d, want 0 (auto rotates per connection already)", got)
	}
	client.profileMu.Lock()
	id := client.stealthProfile.ID
	client.profileMu.Unlock()
	if id != stealth.ProfileIDAuto {
		t.Errorf("stealthProfile = %s, want auto (unchanged)", id)
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"tls handshake failed (wrapper)", errors.New("tls handshake failed: EOF"), true},
		{"tls handshake failure (Go alert)", errors.New("remote error: tls: handshake failure"), true},
		{"tls internal error", errors.New("tls: internal error"), true},
		{"connection refused", errors.New(`dial tcp 127.0.0.1:443: connect: connection refused`), true},
		{"connection reset by peer", errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", errors.New("unexpected EOF"), true},
		{"eof substring not retried", errors.New("peer closed with eof marker"), false},
		{"eof substring wrapped", fmt.Errorf("stealth: tcp dial failed: %w", errors.New("read tcp 1.2.3.4:443: eof reached")), false},
		{"network unreachable", errors.New(`dial tcp 1.2.3.4:443: connect: network is unreachable`), true},
		{"no route to host", errors.New(`dial tcp 1.2.3.4:443: connect: no route to host`), true},
		{"dial i/o timeout", errors.New(`dial tcp 1.2.3.4:443: i/o timeout`), true},
		{"stealth-wrapped connection reset", fmt.Errorf("stealth: tcp dial failed: %w", errors.New("connection reset by peer")), true},
		{"url-wrapped EOF", &url.Error{Op: "Post", URL: "https://www.codebuff.com/api/v1/chat/completions", Err: io.EOF}, true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"tls bad certificate", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), false},
		{"rate limit body", errors.New("upstream rate limited"), false},
		{"arbitrary error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Errorf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNextStealthProfile(t *testing.T) {
	// Deterministic rotation across DISTINCT ClientHelloIDs.
	cases := []struct {
		cur  *stealth.Profile
		want *stealth.Profile
	}{
		{stealth.ProfileChrome120, stealth.ProfileSafari18},
		{stealth.ProfileChrome126, stealth.ProfileSafari18},
		{stealth.ProfileEdge126, stealth.ProfileSafari18},
		{stealth.ProfileSafari17, stealth.ProfileFirefox128},
		{stealth.ProfileSafari18, stealth.ProfileFirefox128},
		{stealth.ProfileFirefox120, stealth.ProfileChrome126},
		{stealth.ProfileFirefox128, stealth.ProfileChrome126},
	}
	for _, tc := range cases {
		if got := nextStealthProfile(tc.cur); got != tc.want {
			t.Errorf("nextStealthProfile(%s) = %s, want %s", tc.cur.ID, got.ID, tc.want.ID)
		}
	}
}

// TestNextStealthProfileUnknownFallback guards the unknown-profile fallback
// (G11): a profile outside the rotation order rotates to the first entry.
func TestNextStealthProfileUnknownFallback(t *testing.T) {
	got := nextStealthProfile(&stealth.Profile{ID: "bogus"})
	if want := retryProfileRotation[0].next; got != want {
		t.Errorf("nextStealthProfile(unknown) = %s, want %s", got.ID, want.ID)
	}
}

// TestWrapDecompressZstdDecoderClosed guards Audit B9 (fix 6): closing a
// zstd-wrapped response body must release the per-response decoder, not just
// the underlying socket (decoder buffers would otherwise linger until GC).
func TestWrapDecompressZstdDecoderClosed(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"status":"active"}`))
	_ = zw.Close()

	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"zstd"}},
		Body:   io.NopCloser(bytes.NewReader(buf.Bytes())),
	}
	if err := wrapDecompress(resp); err != nil {
		t.Fatal(err)
	}
	dc, ok := resp.Body.(*decompressCloser)
	if !ok {
		t.Fatalf("body = %T, want *decompressCloser", resp.Body)
	}
	if dc.closeFn == nil {
		t.Error("zstd decompressCloser has no closeFn: decoder resources leak until GC")
	}
	if _, err := io.ReadAll(dc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := dc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestParseRetryAfterAndFlexTime guards the time parsers' edge branches
// (G2): HTTP-date Retry-After, zero/negative/garbage values, and
// numeric-string unix seconds for flex times.
func TestParseRetryAfterAndFlexTime(t *testing.T) {
	t.Run("http-date", func(t *testing.T) {
		future := time.Now().Add(90 * time.Second).UTC()
		hdr := http.Header{}
		hdr.Set("Retry-After", future.Format(http.TimeFormat))
		got := parseRetryAfter(hdr)
		if got <= 0 || got > 3*time.Minute {
			t.Errorf("HTTP-date Retry-After = %v, want ~90s", got)
		}
	})
	t.Run("seconds, zero, negative, garbage", func(t *testing.T) {
		cases := []struct {
			raw  string
			want time.Duration
		}{
			{"30", 30 * time.Second},
			{"0", 0},
			{"-5", 0},
			{"garbage", 0},
			{"", 0},
		}
		for _, tc := range cases {
			hdr := http.Header{}
			hdr.Set("Retry-After", tc.raw)
			if got := parseRetryAfter(hdr); got != tc.want {
				t.Errorf("Retry-After %q = %v, want %v", tc.raw, got, tc.want)
			}
		}
	})
	t.Run("flex time numeric string seconds", func(t *testing.T) {
		got, err := parseFlexTime("1753075087")
		if err != nil {
			t.Fatalf("parseFlexTime(string seconds): %v", err)
		}
		if want := time.Unix(1753075087, 0); !got.Equal(want) {
			t.Errorf("parseFlexTime = %v, want %v", got, want)
		}
	})
	t.Run("flex time nil and empty error", func(t *testing.T) {
		if _, err := parseFlexTime(nil); err == nil {
			t.Error("parseFlexTime(nil) succeeded")
		}
		if _, err := parseFlexTime(""); err == nil {
			t.Error("parseFlexTime(\"\") succeeded")
		}
	})
}

// TestDoBackoffCancelAndDeadline guards the do() retry-loop branches (G3):
// ctx cancellation during the backoff aborts without a second attempt; a
// pre-existing deadline skips the internal timeout entirely; and after the
// retry budget is exhausted the error surfaces as a non-transient wrap.
func TestDoBackoffCancelAndDeadline(t *testing.T) {
	t.Run("ctx cancel during backoff aborts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		// Block the backoff so cancellation has a window to land in.
		client.retryBackoff = func() time.Duration { return time.Hour }
		rt.failN = 1
		rt.err = errors.New("connection reset by peer")

		ctx, cancel := context.WithCancel(context.Background())
		req, err := client.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := client.do(req, 0)
			done <- err
		}()
		// Wait for the first (failed) attempt, then cancel mid-backoff.
		deadline := time.Now().Add(5 * time.Second)
		for rt.calls.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if rt.calls.Load() != 1 {
			t.Fatalf("first attempt never ran (calls=%d)", rt.calls.Load())
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("do() did not return after cancel during backoff")
		}
		if rt.calls.Load() != 1 {
			t.Errorf("calls = %d after cancel, want 1 (no retry fired)", rt.calls.Load())
		}
	})

	t.Run("pre-existing deadline skips timeout", func(t *testing.T) {
		client, _ := newRetryClient(t, "", 0, "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		req, err := client.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, cfn, err := client.do(req, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if cfn != nil {
			t.Error("timeout applied despite a pre-existing deadline (cancel must be nil)")
		}
	})

	t.Run("exhausted budget returns non-transient wrap", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 2
		rt.err = errors.New("connection reset by peer")
		req, err := client.newRequest(context.Background(), http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = client.do(req, 0)
		if err == nil {
			t.Fatal("expected an error after exhausting the retry budget")
		}
		if !strings.Contains(err.Error(), "upstream:") {
			t.Errorf("err = %v, want an upstream-wrapped error", err)
		}
		if rt.calls.Load() != 2 {
			t.Errorf("calls = %d, want 2 (initial + 1 retry)", rt.calls.Load())
		}
		if got := client.TransientRetries(); got != 1 {
			t.Errorf("TransientRetries = %d, want 1", got)
		}
	})
}

// TestClassifyErrorMatrix guards the 403 classification matrix (G4): the
// narrowed banned marker, deployment_outside_hours precedence across
// statuses, chat-level session markers, 500+rate_limited bodies, and
// ban-before-rate discrimination (E2E flow 3).
func TestClassifyErrorMatrix(t *testing.T) {
	t.Run("banned substring does not over-match", func(t *testing.T) {
		// Regression for Audit B5: a 403 body merely mentioning "banned"
		// (not the {"status":"banned"} marker) must stay a generic 403.
		err := classifyError(403, `{"error":"model temporarily banned from free tier"}`, http.Header{})
		if errors.Is(err, ErrBanned) {
			t.Fatalf("403 with the word banned but no status marker classified as ErrBanned: %v", err)
		}
		var upErr *UpstreamError
		if !errors.As(err, &upErr) || upErr.Status != 403 {
			t.Errorf("err = %v, want a generic 403 UpstreamError", err)
		}
	})

	t.Run("status banned marker still classifies", func(t *testing.T) {
		err := classifyError(403, `{"status":"banned","resumes_at":"2026-07-21T09:18:07+00:00"}`, http.Header{})
		if !errors.Is(err, ErrBanned) {
			t.Errorf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("ban beats rate_limited text", func(t *testing.T) {
		// E2E flow 3: a banned body that also mentions rate_limited text
		// still classifies as a ban (first case wins).
		err := classifyError(403, `{"status":"banned","error":"rate_limited","resumes_at":"2026-07-21T09:18:07+00:00"}`, http.Header{})
		if !errors.Is(err, ErrBanned) {
			t.Errorf("err = %v, want ErrBanned (ban must beat rate text)", err)
		}
	})

	t.Run("deployment_outside_hours preempts status cases", func(t *testing.T) {
		// Pin current behavior (Audit B6, NOT fixed): the marker wins over
		// 401/403/429 classification and yields a retryable UpstreamError.
		cases := []struct {
			name   string
			status int
			body   string
			notErr error
		}{
			{"401", 401, `{"status":"deployment_outside_hours"}`, ErrAuthRejected},
			{"403", 403, `{"status":"deployment_outside_hours"}`, ErrFreeModeCLIRequired},
			{"429", 429, `{"status":"deployment_outside_hours"}`, ErrRateLimited},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := classifyError(tc.status, tc.body, http.Header{})
				var upErr *UpstreamError
				if !errors.As(err, &upErr) {
					t.Fatalf("err = %v, want UpstreamError", err)
				}
				if !upErr.Retryable {
					t.Errorf("deployment_outside_hours not marked Retryable: %v", err)
				}
				if errors.Is(err, tc.notErr) {
					t.Errorf("err = %v, must not classify as %v", err, tc.notErr)
				}
			})
		}
	})

	t.Run("chat-level session markers", func(t *testing.T) {
		err := classifyError(409, `{"status":"model_locked","currentModel":"a","requestedModel":"b"}`, http.Header{})
		if !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("model_locked at chat level = %v, want ErrSessionInvalid", err)
		}
		err = classifyError(400, `{"status":"session_model_mismatch"}`, http.Header{})
		if !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("session_model_mismatch at chat level = %v, want ErrSessionInvalid", err)
		}
	})

	t.Run("500 with rate_limited body", func(t *testing.T) {
		// Pin current behavior: the rate_limited body marker wins even on a
		// 500, producing a RateLimitError.
		err := classifyError(500, `{"status":"rate_limited"}`, http.Header{})
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited", err)
		}
	})

	t.Run("chat-level country blocked", func(t *testing.T) {
		// E2E flow 2: a chat 403 country_blocked body surfaces the typed
		// CountryBlockedError with parsed fields, not a generic 403.
		err := classifyError(403, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`, http.Header{})
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %v, want CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" || cbe.CountryBlockReason != "region_restricted" {
			t.Errorf("country fields = %q/%q, want CN/region_restricted", cbe.CountryCode, cbe.CountryBlockReason)
		}
		if len(cbe.IpPrivacySignals) != 1 || cbe.IpPrivacySignals[0] != "vpn" {
			t.Errorf("ipPrivacySignals = %v, want [vpn]", cbe.IpPrivacySignals)
		}
	})
}

// TestEnsureCliSystemMarkerBranches covers the system-marker merge matrix
// (G5): empty messages, already-present marker, non-string content, merge
// into the first system message, and the unshift path.
func TestEnsureCliSystemMarkerBranches(t *testing.T) {
	t.Run("missing messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{}
		ensureCliSystemMarker(p)
		msgs, ok := p["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", p["messages"])
		}
		sys, ok := msgs[0].(map[string]any)
		if !ok || sys["role"] != "system" || sys["content"] != cliSystemMarker {
			t.Errorf("system message = %v, want role=system with the CLI marker", msgs[0])
		}
	})

	t.Run("empty messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{"messages": []any{}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", msgs)
		}
		if msgs[0].(map[string]any)["content"] != cliSystemMarker {
			t.Errorf("system content = %v", msgs[0])
		}
	})

	t.Run("marker already present in string is untouched", func(t *testing.T) {
		content := cliSystemMarker + "\n\nextra instructions"
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		if got := msgs[0].(map[string]any)["content"]; got != content {
			t.Errorf("system content changed: %v", got)
		}
	})

	t.Run("marker already present in structured parts is untouched", func(t *testing.T) {
		parts := []any{
			map[string]any{"type": "text", "text": cliSystemMarker + " customized"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": parts},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		gotParts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(gotParts) != 1 {
			t.Fatalf("structured parts modified: %v", gotParts)
		}
		if gotParts[0].(map[string]any)["text"] != cliSystemMarker+" customized" {
			t.Errorf("structured text modified: %v", gotParts[0])
		}
	})

	t.Run("structured system content array prepends marker", func(t *testing.T) {
		originalParts := []any{
			map[string]any{"type": "text", "text": "custom instructions"},
			map[string]any{"type": "text", "text": "more instructions"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": originalParts},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		parts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 3 {
			t.Fatalf("system parts = %v, want 3 parts with marker prepended", msgs[0])
		}
		markerPart, ok := parts[0].(map[string]any)
		if !ok || markerPart["type"] != "text" || markerPart["text"] != cliSystemMarker {
			t.Errorf("marker part = %v, want text type with CLI marker", parts[0])
		}
		if parts[1].(map[string]any)["text"] != "custom instructions" || parts[2].(map[string]any)["text"] != "more instructions" {
			t.Errorf("original parts lost: %v", parts)
		}
	})

	t.Run("non-string non-array system content replaced", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": 12345},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("empty string system content replaced with marker", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": ""},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("merges into first system message", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
			map[string]any{"role": "system", "content": "existing"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		sys := msgs[1].(map[string]any)
		if sys["role"] != "system" {
			t.Fatalf("second message = %v, want system", sys)
		}
		if got := sys["content"].(string); !strings.HasPrefix(got, cliSystemMarker) || !strings.Contains(got, "existing") {
			t.Errorf("merged content = %q, want marker + existing", got)
		}
	})

	t.Run("unshifts marker before user", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		if msgs[0].(map[string]any)["role"] != "system" {
			t.Errorf("first message = %v, want system", msgs[0])
		}
	})
}

// TestInjectEnvelopeBranchMatrix covers injectEnvelope's override behavior
// (G5): stream:false is force-overridden, provider is replaced, stop is
// preserved, and a non-object body is rejected.
func TestInjectEnvelopeBranchMatrix(t *testing.T) {
	t.Run("stream false overridden to true", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stream":false}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true {
			t.Errorf("stream = %v, want true", payload["stream"])
		}
	})

	t.Run("provider replaced", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","provider":{"data_collection":"allow"}}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		prov, ok := payload["provider"].(map[string]any)
		if !ok || prov["data_collection"] != "deny" {
			t.Errorf("provider = %v, want data_collection=deny", payload["provider"])
		}
	})

	t.Run("client stop preserved", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stop":["custom"]}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "custom" {
			t.Errorf("stop = %v, want preserved [custom]", payload["stop"])
		}
	})

	t.Run("no stop adds cb_easp", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "cb_easp" {
			t.Errorf("stop = %v, want [cb_easp]", payload["stop"])
		}
	})

	t.Run("non-object body rejected", func(t *testing.T) {
		if _, err := injectEnvelope([]byte(`[1,2,3]`), "free", ChatOptions{RunID: "r"}); err == nil {
			t.Error("injectEnvelope accepted a JSON array body")
		}
	})
}

// TestRequestJitter guards the REQUEST_JITTER gate (G6): the request is held
// before any upstream contact, and canceling during the window aborts with
// context.Canceled and no upstream hit.
func TestRequestJitter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := New("tok", testConfig(mock.URL(), func(c *config.Config) {
		c.RequestJitter = time.Hour
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		done <- err
	}()

	// The jitter gate must hold the request before any upstream contact.
	time.Sleep(50 * time.Millisecond)
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times during the jitter window, want 0", n)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions did not abort on cancel during jitter")
	}
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times after cancel, want 0", n)
	}

	t.Run("small jitter still completes", func(t *testing.T) {
		mock2 := testutil.NewMock()
		defer mock2.Close()
		mock2.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)
		client2, err := New("tok", testConfig(mock2.URL(), func(c *config.Config) {
			c.RequestJitter = 30 * time.Millisecond
		}))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client2.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("chat with jitter failed: %v", err)
		}
		_ = rc.Close()
		if mock2.Requests != 1 {
			t.Errorf("Requests = %d, want 1", mock2.Requests)
		}
	})
}

// TestRedirectMultihop guards multi-hop redirect token semantics (G7): an
// A→B→A loop keeps the token at the origin (B never sees it, A receives its
// own token on the loop-back hop), the 3-hop limit errors out, and a
// port-differing same-host hop is treated as cross-host (token stripped).
func TestRedirectMultihop(t *testing.T) {
	const token = "tok-multihop"

	t.Run("A-B-A loop", func(t *testing.T) {
		bKeySeen := make(chan string, 1)
		aKeySeen := make(chan string, 2)

		var targetBURL string
		originA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, targetBURL+"/b", http.StatusTemporaryRedirect)
			default:
				aKeySeen <- r.Header.Get("x-codebuff-api-key")
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer originA.Close()

		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bKeySeen <- r.Header.Get("x-codebuff-api-key")
			// Loop back to the ORIGIN: Go re-copies headers from via[0]
			// (the origin), so the origin must receive its own token again.
			http.Redirect(w, r, originA.URL+"/loop", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		targetBURL = targetB.URL

		client, err := New(token, testConfig(originA.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		if got := <-bKeySeen; got != "" {
			t.Errorf("intermediate host B received token %q, want stripped", got)
		}
		if got := <-aKeySeen; got != token {
			t.Errorf("loop-back hop to A carried %q, want %q kept", got, token)
		}
	})

	t.Run("three-hop limit", func(t *testing.T) {
		targetC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		}))
		defer targetC.Close()
		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetC.URL+"/c", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetB.URL+"/b", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.http.Do(req)
		if err == nil {
			t.Fatal("3-redirect chain succeeded, want too-many-redirects error")
		}
		if !strings.Contains(err.Error(), "too many redirects") {
			t.Errorf("err = %v, want too many redirects", err)
		}
	})

	t.Run("port-differing same-host strips token", func(t *testing.T) {
		keySeen := make(chan string, 1)
		otherPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keySeen <- r.Header.Get("x-codebuff-api-key")
			w.WriteHeader(http.StatusOK)
		}))
		defer otherPort.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherPort.URL+"/final", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// Pin current behavior: Go's Host includes the port, so a different
		// port is treated as cross-host and the token is dropped.
		if got := <-keySeen; got != "" {
			t.Errorf("port-differing hop carried token %q, want stripped", got)
		}
	})
}

// TestNewWithIndexPrecedence guards the proxy precedence combos (G8):
// SOCKS5_PROXIES > SOCKS5_PROXY > HTTP_PROXY, and the winner disables the
// env-proxy path.
func TestNewWithIndexPrecedence(t *testing.T) {
	t.Run("socks proxies beat single socks proxy", func(t *testing.T) {
		client, err := NewWithIndex("tok", 0, testConfig("", func(c *config.Config) {
			c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
			c.SOCKS5Proxy = "socks5://127.0.0.1:9999"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(client.socksProxies) != 2 || len(client.socksDialers) != 2 {
			t.Fatalf("socksProxies = %v, want the SOCKS5_PROXIES list", client.socksProxies)
		}
		tr := client.http.Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Error("env HTTP proxy not disabled when SOCKS5_PROXIES wins")
		}
		if !tr.DisableKeepAlives {
			t.Error("multi-proxy rotation should disable keep-alives")
		}
	})

	t.Run("socks proxies beat http proxy", func(t *testing.T) {
		client, err := NewWithIndex("tok", 0, testConfig("", func(c *config.Config) {
			c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001"}
			c.HTTPProxy = "http://127.0.0.1:9999"
		}))
		if err != nil {
			t.Fatal(err)
		}
		tr := client.http.Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Error("HTTP_PROXY applied even though SOCKS5_PROXIES wins")
		}
	})

	t.Run("single socks proxy beats http proxy", func(t *testing.T) {
		client, err := NewWithIndex("tok", 0, testConfig("", func(c *config.Config) {
			c.SOCKS5Proxy = "socks5://127.0.0.1:9999"
			c.HTTPProxy = "http://127.0.0.1:9998"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(client.socksProxies) != 0 {
			t.Errorf("socksProxies = %v, want empty for the singular SOCKS5_PROXY", client.socksProxies)
		}
		tr := client.http.Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Error("HTTP_PROXY applied even though SOCKS5_PROXY wins")
		}
		if tr.DialContext == nil {
			t.Error("SOCKS5 dialer not wired into DialContext")
		}
	})
}

// socks5TestServer is a minimal RFC 1928 SOCKS5 server (optional RFC 1929
// username/password auth) used to observe which proxy actually gets dialed
// and which credentials arrive.
type socks5TestServer struct {
	ln          net.Listener
	requireAuth bool
	user, pass  string

	mu        sync.Mutex
	conns     int
	gotUser   string
	gotPass   string
	authFails int
}

func newSocks5TestServer(t *testing.T, requireAuth bool, user, pass string) *socks5TestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}
	s := &socks5TestServer{ln: ln, requireAuth: requireAuth, user: user, pass: pass}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *socks5TestServer) Addr() string { return s.ln.Addr().String() }

func (s *socks5TestServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *socks5TestServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if s.requireAuth {
		if _, err := c.Write([]byte{5, 2}); err != nil {
			return
		}
		ahdr := make([]byte, 2)
		if _, err := io.ReadFull(br, ahdr); err != nil || ahdr[0] != 1 {
			return
		}
		uname := make([]byte, int(ahdr[1]))
		if _, err := io.ReadFull(br, uname); err != nil {
			return
		}
		phdr := make([]byte, 1)
		if _, err := io.ReadFull(br, phdr); err != nil {
			return
		}
		pw := make([]byte, int(phdr[0]))
		if _, err := io.ReadFull(br, pw); err != nil {
			return
		}
		s.mu.Lock()
		s.gotUser, s.gotPass = string(uname), string(pw)
		s.mu.Unlock()
		if string(uname) != s.user || string(pw) != s.pass {
			s.mu.Lock()
			s.authFails++
			s.mu.Unlock()
			_, _ = c.Write([]byte{1, 1})
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return
		}
	} else if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 3)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 5 || req[1] != 1 {
		return
	}
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(br, atyp); err != nil {
		return
	}
	var host string
	switch atyp[0] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(br, port); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))

	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(up, br)
		done <- struct{}{}
	}()
	_, _ = io.Copy(c, up)
	<-done
}

// TestSocks5UserinfoDialThrough is the regression test for Audit B2 (fix 4):
// an authenticated socks5://user:pass@ URL must actually authenticate against
// a proxy that requires it, and the credentials must arrive intact.
func TestSocks5UserinfoDialThrough(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	srv := newSocks5TestServer(t, true, "alice", "s3cret")
	client, err := New("tok", testConfig(mock.URL(), func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://alice:s3cret@" + srv.Addr()}
	}))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("chat through authenticated proxy failed: %v", err)
	}
	_ = rc.Close()

	srv.mu.Lock()
	gotUser, gotPass, conns, fails := srv.gotUser, srv.gotPass, srv.conns, srv.authFails
	srv.mu.Unlock()
	if gotUser != "alice" || gotPass != "s3cret" {
		t.Errorf("proxy saw auth %q/%q, want alice/s3cret (userinfo must reach the SOCKS5 handshake)", gotUser, gotPass)
	}
	if conns != 1 || fails != 0 {
		t.Errorf("proxy conns=%d authFails=%d, want 1/0", conns, fails)
	}
}

// TestProxyRotationActualConnections exercises PROXY_ROTATION end to end
// (E2E flow 4): with round-robin across two SOCKS5 proxies, each chat
// request actually dials a different proxy (DisableKeepAlives forces the
// re-dial), and every request reaches the upstream.
func TestProxyRotationActualConnections(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	s1 := newSocks5TestServer(t, false, "", "")
	s2 := newSocks5TestServer(t, false, "", "")
	client, err := New("tok", testConfig(mock.URL(), func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://" + s1.Addr(), "socks5://" + s2.Addr()}
		c.ProxyRotation = "round-robin"
	}))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("chat through rotated proxy failed: %v", err)
		}
		_ = rc.Close()
	}
	s1.mu.Lock()
	c1 := s1.conns
	s1.mu.Unlock()
	s2.mu.Lock()
	c2 := s2.conns
	s2.mu.Unlock()
	if c1 != 1 || c2 != 1 {
		t.Errorf("proxy connections = %d/%d, want 1/1 (each request must land on a different proxy)", c1, c2)
	}
	if mock.Requests != 2 {
		t.Errorf("upstream requests = %d, want 2", mock.Requests)
	}
}

// TestProxyIndexForZeroProxies guards the division-by-zero fix (fix 3, Audit
// B4): proxyIndexFor must not panic with zero proxies, with or without a
// stashed (out-of-range) index in the context.
func TestProxyIndexForZeroProxies(t *testing.T) {
	client, err := New("tok", testConfig("", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := client.proxyIndexFor(context.Background()); got != 0 {
		t.Errorf("proxyIndexFor(bare ctx) = %d, want 0", got)
	}
	stashed := context.WithValue(context.Background(), proxyIndexKey{}, 7)
	if got := client.proxyIndexFor(stashed); got != 0 {
		t.Errorf("proxyIndexFor(stashed 7, no proxies) = %d, want 0 (no panic)", got)
	}
}

// TestFailedReplayMetricsDisagreement pins the current metrics behavior on a
// failed body replay (Audit B3, NOT fixed): the pinned fingerprint is rotated
// and counted BEFORE the replay, so a failed replay leaves FingerprintRotations
// incremented with no TransientRetries and the profile permanently swapped.
func TestFailedReplayMetricsDisagreement(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "chrome126")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://example.com/api/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("replay body unavailable") }

	// With the replay failing, do() surfaces the transient error directly
	// (no second attempt fires): the rotation already happened, the retry
	// did not.
	_, cfn, err := client.do(req, 0)
	if err == nil {
		t.Fatal("expected the transient error to surface when the body cannot be replayed")
	}
	if cfn != nil {
		defer cfn()
	}

	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1 (rotation precedes the replay)", got)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0 (no retry fired)", got)
	}
	if got := client.currentStealthProfile().ID; got != stealth.ProfileIDSafari18 {
		t.Errorf("profile after failed replay = %s, want %s (permanently rotated)", got, stealth.ProfileIDSafari18)
	}
}

// TestSessionCallUnknownStatus5xx pins current sessionCall behavior (G10):
// any status code with a parseable body carrying a non-empty status field
// yields a SessionState, not an error — even a 5xx with an unknown status.
func TestSessionCallUnknownStatus5xx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"weird","message":"unknown status"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for a parseable 5xx body: %v", err)
	}
	if st.Status != "weird" {
		t.Errorf("status = %q, want weird", st.Status)
	}
}

// TestEndSession404Tolerated guards the EndSession 404 contract (E2E flow
// 10): a 404 DELETE is "nothing to end", not an error, while a 5xx is.
func TestEndSession404Tolerated(t *testing.T) {
	t.Run("404 tolerated", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background(), "inst-1"); err != nil {
			t.Errorf("EndSession 404 = %v, want nil", err)
		}
	})

	t.Run("5xx surfaces error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background(), "inst-1"); err == nil {
			t.Error("EndSession 500 succeeded, want error")
		}
	})
}

// TestClassify429ChatLevel guards 429 ip_capped/spend_limited bodies at the
// chat level (G10): they classify as RateLimitError carrying the status.
func TestClassify429ChatLevel(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus string
	}{
		{"ip_capped", `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`, "ip_capped"},
		{"spend_limited", `{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`, "spend_limited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(http.StatusTooManyRequests, tc.body, http.Header{})
			var rle *RateLimitError
			if !errors.As(err, &rle) {
				t.Fatalf("err = %v, want RateLimitError", err)
			}
			if !errors.Is(err, ErrRateLimited) {
				t.Errorf("err = %v, want ErrRateLimited", err)
			}
			if rle.Status != tc.wantStatus {
				t.Errorf("RateLimitError.Status = %q, want %q", rle.Status, tc.wantStatus)
			}
		})
	}
}

// TestChatNonObjectBodyAndGzipError guards G12: a non-object chat body is
// rejected at the envelope stage, and a gzip-compressed 4xx error body is
// drained and decompressed before classification.
func TestChatNonObjectBodyAndGzipError(t *testing.T) {
	t.Run("non-object body rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`[1,2,3]`))
		if err == nil {
			t.Fatal("array chat body accepted, want envelope error")
		}
		if !strings.Contains(err.Error(), "envelope") {
			t.Errorf("err = %v, want an envelope error", err)
		}
		if mock.Requests != 0 {
			t.Errorf("upstream hit %d times for a rejected body, want 0", mock.Requests)
		}
	})

	t.Run("gzip 4xx body decompressed before classify", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusTooManyRequests)
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(`{"status":"rate_limited","retryAfterMs":60000}`))
			_ = zw.Close()
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited (gzip body must be decompressed before classification)", err)
		}
	})
}

// TestChatRetriesRealDialFailure is E2E flow 1: a real transport failure
// (the listener accepts then hangs up mid-request) is retried over a fresh
// connection with a byte-identical replayed body, and the SSE stream reads
// back cleanly.
func TestChatRetriesRealDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	bodies := make(chan []byte, 2)
	sse := testutil.SSEEvent(`{"id":"c0","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		"data: [DONE]\n\n"

	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(conn)
			req, err := http.ReadRequest(br)
			if err != nil {
				// Keep accepting: the retry still needs a second connection.
				_ = conn.Close()
				continue
			}
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			bodies <- body
			if i == 0 {
				// Swallow the first request, then hang up: the client sees
				// a transport-level EOF and must retry.
				_ = conn.Close()
				continue
			}
			resp := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: " +
				strconv.Itoa(len(sse)) + "\r\nConnection: close\r\n\r\n" + sse
			_, _ = conn.Write([]byte(resp))
			_ = conn.Close()
		}
	}()

	client, err := New("tok", testConfig("http://"+ln.Addr().String(), func(c *config.Config) {
		c.TransientRetries = 1
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("chat failed after retry: %v", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"hi"`) {
		t.Errorf("stream missing expected chunk: %s", data)
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}

	first := <-bodies
	second := <-bodies
	if !bytes.Equal(first, second) {
		t.Errorf("replayed body differs:\n first: %s\nsecond: %s", first, second)
	}
}

// TestRetryRotatesFingerprintAtDialLayer is E2E flow 5: after a transient
// retry, the transport dials with the rotated profile's ClientHello — the
// dial-layer profile capture must show chrome126 then safari18.
func TestRetryRotatesFingerprintAtDialLayer(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
	}))
	defer tlsSrv.Close()

	client, err := New("tok", testConfig(tlsSrv.URL, func(c *config.Config) {
		c.TLSFingerprint = "chrome126"
		c.TransientRetries = 1
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	tr := client.http.Transport.(*http.Transport)
	var mu sync.Mutex
	var dials []stealth.ProfileID
	var dialCount int
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		prof := client.dialProfileFor(ctx)
		mu.Lock()
		dialCount++
		dials = append(dials, prof.ID)
		mu.Unlock()
		if dialCount == 1 {
			return nil, errors.New("tls handshake failed: injected first-dial failure")
		}
		if prof.ID == stealth.ProfileIDSafari18 {
			// The package-level Safari profile shares one mutable
			// utls.ClientHelloSpec that the first utls handshake corrupts
			// (KeyShareExtension data is written in place), so a second
			// real dial through the shared spec is flaky (pre-existing
			// stealth bug, reported separately). Dial with utls's own
			// built-in Safari preset instead — still a Safari-family
			// fingerprint — while the rotation DECISION (chrome126 ->
			// safari18) is pinned by the captured profile IDs.
			p := *prof
			p.ClientHelloID = utls.HelloSafari_16_0
			p.CustomSpec = nil
			prof = &p
		}
		// Production hard-codes InsecureSkipVerify=false; the local test
		// server's self-signed cert requires true.
		return stealth.Dialer(prof, nil, true)(ctx, network, addr)
	}

	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 2 || dials[0] != stealth.ProfileIDChrome126 || dials[1] != stealth.ProfileIDSafari18 {
		t.Errorf("dialed profiles = %v, want [chrome126 safari18]", dials)
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1", got)
	}
}

// TestConnectTunnelStealthRealSockets is E2E flow 6: HTTP_PROXY with
// credentials plus a pinned TLS fingerprint routes the origin TLS through a
// CONNECT tunnel over real sockets, authenticating to the proxy.
func TestConnectTunnelStealthRealSockets(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"tunneled"},"finish_reason":null}]}`))
	}))
	defer origin.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	proxyAddr := ln.Addr().String()
	authSeen := make(chan string, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				authSeen <- req.Header.Get("Proxy-Authorization")
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				done := make(chan struct{}, 1)
				go func() {
					_, _ = io.Copy(up, br)
					done <- struct{}{}
				}()
				_, _ = io.Copy(c, up)
				<-done
			}(c)
		}
	}()

	proxyURL := "http://user:pass@" + proxyAddr
	client, err := New("tok", testConfig(origin.URL, func(c *config.Config) {
		c.HTTPProxy = proxyURL
		c.TLSFingerprint = "chrome126"
	}))
	if err != nil {
		t.Fatal(err)
	}
	tr := client.http.Transport.(*http.Transport)
	pu, _ := url.Parse(proxyURL)
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		prof := client.dialProfileFor(ctx)
		// InsecureSkipVerify=true only because the local origin is
		// self-signed; production hard-codes false.
		return stealth.Dialer(prof, httpConnectDial(pu), true)(ctx, network, addr)
	}

	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("chat through CONNECT tunnel failed: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"tunneled"`) {
		t.Errorf("stream missing tunneled chunk: %s", data)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if got := <-authSeen; got != wantAuth {
		t.Errorf("CONNECT Proxy-Authorization = %q, want %q", got, wantAuth)
	}
}

// TestFullChatLifecycleChained is E2E flow 7: create session, start run,
// chat (with instance-id + envelope), finish run, end session — in one
// chain, asserting the instance/run ids thread through.
func TestFullChatLifecycleChained(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lifecycle"},"finish_reason":null}]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, err := client.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if st.Status != "active" || st.InstanceID == "" {
		t.Fatalf("session = %+v, want active with an instance id", st)
	}

	runID, err := client.StartRun(ctx, "agent-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if runID == "" {
		t.Fatal("StartRun returned an empty run id")
	}

	rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: runID, SessionInstanceID: st.InstanceID},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"lifecycle"`) {
		t.Errorf("stream missing chunk: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("recorded chat headers = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-instance-id"); got != st.InstanceID {
		t.Errorf("chat x-freebuff-instance-id = %q, want %q", got, st.InstanceID)
	}
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "m" {
		t.Errorf("chat x-freebuff-model = %q, want m", got)
	}
	if !mock.BodyContains(`"freebuff_instance_id":"` + st.InstanceID + `"`) {
		t.Error("chat body missing freebuff_instance_id in codebuff_metadata")
	}
	if !mock.BodyContains(`"run_id":"` + runID + `"`) {
		t.Error("chat body missing run_id in codebuff_metadata")
	}

	if err := client.FinishRun(ctx, runID, 3); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := client.EndSession(ctx, st.InstanceID); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	if mock.SessionCreates != 1 || mock.SessionEnds != 1 {
		t.Errorf("session creates/ends = %d/%d, want 1/1", mock.SessionCreates, mock.SessionEnds)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("started runs = %v, want [agent-1]", got)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != runID || finished[0].TotalSteps != 3 {
		t.Errorf("finished runs = %+v, want run %s with 3 steps", finished, runID)
	}
}

// TestCompactHeartbeatAbsentTolerant is E2E flow 8: a compact heartbeat poll
// without quota/offer fields parses cleanly with nil maps.
func TestCompactHeartbeatAbsentTolerant(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	gotCompact := make(chan string, 1)
	gotHeartbeat := make(chan string, 1)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact <- r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat <- r.Header.Get("x-freebuff-heartbeat")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-08-17T10:00:00.000Z"}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true, true)
	if err != nil {
		t.Fatalf("compact heartbeat poll: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("state = %+v, want active inst-1", st)
	}
	if st.RateLimitsByModel != nil {
		t.Errorf("RateLimitsByModel = %v, want nil on a compact poll without quotas", st.RateLimitsByModel)
	}
	if st.LimitedModelOffers != nil {
		t.Errorf("LimitedModelOffers = %v, want nil on a compact poll without offers", st.LimitedModelOffers)
	}
	if got := <-gotCompact; got != "1" {
		t.Errorf("compact header = %q, want 1", got)
	}
	if got := <-gotHeartbeat; got != "1" {
		t.Errorf("heartbeat header = %q, want 1", got)
	}
}
