package telemetry

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// closeLogFile closes the log file held by a NewLogger result so TempDir
// cleanup can delete it (Windows refuses to delete open files).
func closeLogFile(t *testing.T, logger *slog.Logger) {
	t.Helper()
	th, ok := logger.Handler().(*textHandler)
	if !ok {
		t.Fatalf("logger handler is %T, want *textHandler", logger.Handler())
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.file != nil {
		_ = th.file.Close()
		th.file = nil
	}
}

// captureStderr reroutes os.Stderr for the duration of fn and returns
// everything written to it. Not safe under t.Parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func TestNewLoggerLevelSelection(t *testing.T) {
	infoFile := filepath.Join(t.TempDir(), "info.log")
	infoLogger := NewLogger(false, infoFile)
	infoLogger.Debug("debug line")
	infoLogger.Info("info line")
	closeLogFile(t, infoLogger)

	data, err := os.ReadFile(infoFile)
	if err != nil {
		t.Fatalf("read %s: %v", infoFile, err)
	}
	got := string(data)
	if !strings.Contains(got, `msg="info line"`) {
		t.Errorf("Info line missing from log file: %q", got)
	}
	if strings.Contains(got, "debug line") {
		t.Errorf("Debug line logged at Info level: %q", got)
	}

	debugFile := filepath.Join(t.TempDir(), "debug.log")
	debugLogger := NewLogger(true, debugFile)
	debugLogger.Debug("debug line")
	closeLogFile(t, debugLogger)

	data, err = os.ReadFile(debugFile)
	if err != nil {
		t.Fatalf("read %s: %v", debugFile, err)
	}
	if !strings.Contains(string(data), `msg="debug line"`) {
		t.Errorf("Debug line missing at Debug level: %q", data)
	}
}

func TestNewLoggerAppendsToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	first := NewLogger(true, path)
	first.Info("first")
	second := NewLogger(true, path)
	second.Info("second")
	closeLogFile(t, first)
	closeLogFile(t, second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, "msg=first") || !strings.Contains(got, "msg=second") {
		t.Errorf("expected both lines appended, got: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("log file contains ANSI color escapes: %q", got)
	}
}

func TestNewLoggerColors(t *testing.T) {
	out := captureStderr(t, func() {
		logger := NewLogger(true, "")
		logger.Debug("m-debug")
		logger.Info("m-info")
		logger.Warn("m-warn")
		logger.Error("m-error")
	})
	for _, want := range []struct{ msg, token string }{
		{"m-debug", "\x1b[90mDEBUG\x1b[0m"},
		{"m-info", "\x1b[32mINFO\x1b[0m"},
		{"m-warn", "\x1b[33mWARN\x1b[0m"},
		{"m-error", "\x1b[31mERROR\x1b[0m"},
	} {
		if !strings.Contains(out, want.msg) {
			t.Errorf("stderr missing message %q", want.msg)
		}
		if !strings.Contains(out, want.token) {
			t.Errorf("stderr missing color token %q (for %s): %q", want.token, want.msg, out)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer topsecret")
	h.Add("x-api-key", "key-1")
	h.Add("x-api-key", "key-2")
	h.Set("X-Codebuff-Api-Key", "cb_secret")
	h.Set("Cookie", "session=abc")
	h.Set("Set-Cookie", "sid=xyz")
	h.Set("Content-Type", "application/json")
	h.Set("X-Custom", "keepme")

	got := RedactHeaders(h)

	if h.Get("Authorization") != "Bearer topsecret" {
		t.Error("RedactHeaders modified the input header")
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Codebuff-Api-Key", "Cookie", "Set-Cookie"} {
		if v := got[k][0]; v != "[redacted]" {
			t.Errorf("RedactHeaders[%q] = %q, want [redacted]", k, v)
		}
	}
	if v := got["Content-Type"][0]; v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	if v := got["X-Custom"][0]; v != "keepme" {
		t.Errorf("X-Custom = %q, want keepme", v)
	}
	if vs := got["X-Api-Key"]; len(vs) != 2 || vs[0] != "[redacted]" || vs[1] != "[redacted]" {
		t.Errorf("X-Api-Key values = %v, want two [redacted]", vs)
	}
}

func TestRedactHeadersNonCanonicalKey(t *testing.T) {
	h := http.Header{"x-api-key": {"k"}}
	got := RedactHeaders(h)
	if v := got["x-api-key"][0]; v != "[redacted]" {
		t.Errorf("lowercase raw key value = %q, want [redacted]", v)
	}
}

func TestParseLevel(t *testing.T) {
	if _, ok := ParseLevel(""); ok {
		t.Error(`ParseLevel("") ok=true, want false`)
	}
	if lv, ok := ParseLevel("debug"); !ok || lv != slog.LevelDebug {
		t.Errorf("ParseLevel(debug) = %v, ok=%v; want %v, true", lv, ok, slog.LevelDebug)
	}
	if lv, ok := ParseLevel("INFO"); !ok || lv != slog.LevelInfo {
		t.Errorf("ParseLevel(INFO) = %v, ok=%v; want %v, true", lv, ok, slog.LevelInfo)
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Error("ParseLevel(bogus) ok=true, want false")
	}
}

func TestSanitizeName(t *testing.T) {
	input := `a\b:c*d?e"f<g>h|i`
	got := sanitizeName(input)
	for _, r := range got {
		if strings.ContainsRune(`/\:*?"<>|.`, r) {
			t.Errorf("sanitizeName(%q) = %q, contains invalid file-name char %q", input, got, r)
		}
	}
	if len(got) > 60 {
		t.Errorf("sanitizeName(%q) length = %d, want <= 60", input, len(got))
	}
}

func TestTruncateUTF8Safe(t *testing.T) {
	// 50 multi-byte runes (100 bytes): byte slicing would split a rune.
	s := strings.Repeat("é", 50)
	got := truncate(s, 10)
	if n := len([]rune(got)); n != 13 {
		t.Errorf("truncate(50×é, 10) = %d runes, want 13 (10 + ellipsis)", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate(50×é, 10) = %q, want ellipsis suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate(50×é, 10) = %q, want valid UTF-8", got)
	}

	if short := truncate("abc", 5); short != "abc" {
		t.Errorf("truncate(abc, 5) = %q, want abc (unchanged)", short)
	}
}

// TestNewLoggerCreatesNestedLogDir verifies that LOG_FILE may point into a
// fresh nested directory: the parent is created before the file is opened.
func TestNewLoggerCreatesNestedLogDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "proxy.log")
	logger := NewLogger(true, path)
	logger.Info("nested-dir line")
	closeLogFile(t, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (parent dir was not created)", path, err)
	}
	if !strings.Contains(string(data), `msg="nested-dir line"`) {
		t.Errorf("line missing from log file in nested dir: %q", data)
	}
}

func TestColorizeWhenLogFileFailsToOpen(t *testing.T) {
	// A log file whose parent cannot be created (a regular file occupies the
	// directory position, so MkdirAll fails) cannot be opened; the logger
	// falls back to stderr-only and must keep its ANSI colors.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		logger := NewLogger(true, filepath.Join(blocker, "x.log"))
		logger.Info("still-colored")
	})
	if !strings.Contains(out, "\x1b[32mINFO\x1b[0m") {
		t.Errorf("stderr lost colors after log file open failure: %q", out)
	}
}

// TestAttrValueEscaping verifies that attr values containing newlines, tabs,
// quotes or other control characters are quoted, so one log record always
// writes exactly one line. Regression: values were concatenated via
// a.Value.String() unescaped, so a client-controlled model name or URL path
// containing "\nlevel=ERROR injected" forged a second log line.
func TestAttrValueEscaping(t *testing.T) {
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: slog.LevelInfo}
	logger := slog.New(h)
	logger.Info("request handled",
		"model", "codebuff-1\nlevel=ERROR injected",
		"path", "/v1/chat/completions",
		"user_agent", "tab\t\"quoted\"\x1b[31mred",
	)

	out := buf.String()
	// The injected token must survive only in its strconv.Quote-escaped form
	// inside the quoted value, never as a raw newline splitting the record.
	if !strings.Contains(out, `model="codebuff-1\nlevel=ERROR injected"`) {
		t.Errorf("model attr not escaped via strconv.Quote, got: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("record split across %d lines, want exactly 1: %q", n, out)
	}
	if !strings.Contains(out, `user_agent="tab\t\"quoted\"\x1b[31mred"`) {
		t.Errorf("tab/quote/control chars not escaped, got: %q", out)
	}
	if !strings.Contains(out, "path=/v1/chat/completions") {
		t.Errorf("clean attr value must stay unquoted, got: %q", out)
	}
}

// TestWithAttrsWithGroupNoOp pins the text handler's no-op
// WithAttrs/WithGroup contract: both return the SAME handler, and bound
// attrs/groups are silently dropped from the output (the process logger
// never carries them — a regression would double-print fields).
func TestWithAttrsWithGroupNoOp(t *testing.T) {
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: slog.LevelInfo}
	if got := h.WithAttrs([]slog.Attr{slog.String("k", "v")}); got != slog.Handler(h) {
		t.Errorf("WithAttrs returned a different handler: %T", got)
	}
	if got := h.WithGroup("grp"); got != slog.Handler(h) {
		t.Errorf("WithGroup returned a different handler: %T", got)
	}
	logger := slog.New(h).With("bound", "attr").WithGroup("grp")
	logger.Info("msg")
	out := buf.String()
	if strings.Contains(out, "bound=attr") {
		t.Errorf("bound attr leaked into output despite no-op WithAttrs: %q", out)
	}
	if strings.Contains(out, "grp.") {
		t.Errorf("group prefix leaked into output despite no-op WithGroup: %q", out)
	}
	if !strings.Contains(out, "msg=msg") {
		t.Errorf("plain record missing: %q", out)
	}
}

// TestQuoteMessageEdges pins the quoting decision table: multi-word
// messages and values with tabs/newlines/CRs/quotes/control characters are
// strconv-quoted; single tokens, empty strings and non-control Unicode are
// not.
func TestQuoteMessageEdges(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool // needsQuote
	}{
		{"empty", "", false},
		{"single token", "hello", false},
		{"space", "two words", true},
		{"tab", "a\tb", true},
		{"newline", "a\nb", true},
		{"carriage return", "a\rb", true},
		{"double quote", `a"b`, true},
		{"control char", "a\x01b", true},
		{"non-control unicode single token", "héllo", false},
		{"non-control unicode with space", "héllo wörld", true},
		{"narrow no-break space", "a\u00a0b", false},
		{"emoji", "🚀", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsQuote(tc.s); got != tc.want {
				t.Errorf("needsQuote(%q) = %v, want %v", tc.s, got, tc.want)
			}
			// quoteMessage must round-trip: quoted iff needsQuote.
			if quoted := quoteMessage(tc.s) != tc.s; quoted != tc.want {
				t.Errorf("quoteMessage(%q) quoted=%v, want %v", tc.s, quoted, tc.want)
			}
		})
	}
}

// TestSanitizeNameBoundary pins the 60-rune truncation boundary and the
// character replacement: shorter names are unchanged, exactly-60 stays 60,
// longer names are cut to 60, invalid file-name characters are replaced with
// underscores, and multi-byte input is truncated by runes so no UTF-8
// sequence is ever split.
func TestSanitizeNameBoundary(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"empty", "", 0},
		{"short", "chat-1", 6},
		{"exactly 60", strings.Repeat("a", 60), 60},
		{"61 truncated", strings.Repeat("a", 61), 60},
		{"long with specials", strings.Repeat("a/b:c", 20), 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.in)
			if len(got) != tc.wantLen {
				t.Errorf("sanitizeName(%q) length = %d, want %d", tc.in, len(got), tc.wantLen)
			}
		})
	}
	if got := sanitizeName(`a\b:c*d?e"f<g>h|i.`); got != "a_b_c_d_e_f_g_h_i_" {
		t.Errorf("sanitizeName(specials) = %q", got)
	}

	// 61 two-byte runes (122 bytes): a byte slice cut at 60 would split a
	// rune and emit invalid UTF-8; rune-based truncation yields 60 whole
	// runes that are valid UTF-8.
	multi := strings.Repeat("é", 61)
	got := sanitizeName(multi)
	if n := len([]rune(got)); n != 60 {
		t.Errorf("sanitizeName(61×é) = %d runes, want 60", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeName(61×é) produced invalid UTF-8: %q", got)
	}
}

// TestLevelColorBelowDebug pins the ANSI level palette: anything below
// DEBUG renders gray, DEBUG gray, INFO green, WARN yellow, ERROR and above
// red.
func TestLevelColorBelowDebug(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug - 8, ansiGray},
		{slog.LevelDebug, ansiGray},
		{slog.LevelInfo, ansiGreen},
		{slog.LevelWarn, ansiYellow},
		{slog.LevelError, ansiRed},
		{slog.LevelError + 8, ansiRed},
	}
	for _, tc := range cases {
		if got := levelColor(tc.level); got != tc.want {
			t.Errorf("levelColor(%v) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// TestRedactHeadersNilEmpty pins the nil/empty inputs: RedactHeaders of a
// nil or empty header returns an empty (non-nil) map.
func TestRedactHeadersNilEmpty(t *testing.T) {
	if got := RedactHeaders(nil); len(got) != 0 {
		t.Errorf("RedactHeaders(nil) = %v, want empty", got)
	}
	if got := RedactHeaders(http.Header{}); len(got) != 0 {
		t.Errorf("RedactHeaders(empty) = %v, want empty", got)
	}
}
