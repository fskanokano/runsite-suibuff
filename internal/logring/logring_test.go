package logring

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// discarding is a sink that accepts everything and keeps nothing.
type discarding struct{}

func (discarding) Enabled(context.Context, slog.Level) bool  { return true }
func (discarding) Handle(context.Context, slog.Record) error { return nil }
func (d discarding) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discarding) WithGroup(string) slog.Handler           { return d }

func TestRingRetainsNewestFirst(t *testing.T) {
	h := NewHandler(discarding{}, 3)
	logger := slog.New(h)
	for i := range 5 {
		logger.Info("msg", "n", i)
	}
	recent := h.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("Recent(10) = %d entries, want 3 (bounded capacity)", len(recent))
	}
	// Newest first: n=4, n=3, n=2.
	for i, want := range []string{"n=4", "n=3", "n=2"} {
		if recent[i].Message != "msg" {
			t.Fatalf("entry %d message = %q", i, recent[i].Message)
		}
		found := false
		for _, f := range recent[i].Fields {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("entry %d fields %v missing %q", i, recent[i].Fields, want)
		}
	}
}

func TestRingSubHandlersShareStore(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h)
	sub := logger.With("scope", "pool")
	sub.Info("from sub")
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	if recent[0].Message != "from sub" {
		t.Errorf("message = %q, want %q", recent[0].Message, "from sub")
	}
	found := false
	for _, f := range recent[0].Fields {
		if f == "scope=pool" {
			found = true
		}
	}
	if !found {
		t.Errorf("bound attr not retained: %v", recent[0].Fields)
	}
}

func TestRingForwardsToNext(t *testing.T) {
	var got string
	next := slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		got += string(p)
		return len(p), nil
	}), nil)
	h := NewHandler(next, 5)
	slog.New(h).Info("hello")
	if got == "" {
		t.Error("record was not forwarded to the wrapped handler")
	}
}

// TestRingFlattenGroupKeys verifies grouped attrs keep the group key as a
// dotted prefix: slog.Group("http", slog.Int("status", 200)) must render as
// "http.status=200", not "status.status=200" (the child key must not replace
// the group key it extends).
func TestRingFlattenGroupKeys(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	slog.New(h).Info("msg",
		slog.Group("http", slog.Int("status", 200)),
		slog.Group("nested", slog.Group("deep", slog.String("k", "v"))),
	)
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	found := map[string]bool{}
	for _, f := range recent[0].Fields {
		found[f] = true
	}
	for _, want := range []string{"http.status=200", "nested.deep.k=v"} {
		if !found[want] {
			t.Errorf("fields %v missing %q", recent[0].Fields, want)
		}
	}
	if found["status=200"] {
		t.Errorf("fields %v contain %q: group key was dropped", recent[0].Fields, "status=200")
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// levelGate is a sink that only accepts records at or above its level.
type levelGate struct{ level slog.Level }

func (g levelGate) Enabled(_ context.Context, l slog.Level) bool { return l >= g.level }
func (g levelGate) Handle(context.Context, slog.Record) error    { return nil }
func (g levelGate) WithAttrs([]slog.Attr) slog.Handler           { return g }
func (g levelGate) WithGroup(string) slog.Handler                { return g }

// errorSink is a sink that always returns an error from Handle.
type errorSink struct{}

func (errorSink) Enabled(context.Context, slog.Level) bool  { return true }
func (errorSink) Handle(context.Context, slog.Record) error { return errors.New("sink boom") }
func (e errorSink) WithAttrs([]slog.Attr) slog.Handler      { return e }
func (e errorSink) WithGroup(string) slog.Handler           { return e }

// TestWithAttrsWithGroupFold verifies combined WithGroup+WithAttrs: the
// group name becomes the dotted prefix on BOTH bound attrs and the
// record's own grouped attrs (svc.node=n1, svc.http.status=200), and no
// double-prefixed or unprefixed variants leak through.
func TestWithAttrsWithGroupFold(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h).WithGroup("svc").With("node", "n1")
	logger.Info("msg", slog.Group("http", slog.Int("status", 200)))
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	found := map[string]bool{}
	for _, f := range recent[0].Fields {
		found[f] = true
	}
	for _, want := range []string{"svc.node=n1", "svc.http.status=200"} {
		if !found[want] {
			t.Errorf("fields %v missing %q", recent[0].Fields, want)
		}
	}
	for _, bad := range []string{"node=n1", "http.status=200", "svc.svc.node=n1"} {
		if found[bad] {
			t.Errorf("fields %v contain %q (group fold wrong)", recent[0].Fields, bad)
		}
	}
}

// TestEnabledGating verifies gating is forwarded to the wrapped handler: a
// record the sink rejects at Enabled is never retained in the ring.
func TestEnabledGating(t *testing.T) {
	h := NewHandler(levelGate{level: slog.LevelInfo}, 10)
	logger := slog.New(h)
	logger.Debug("hidden")
	logger.Info("shown")
	recent := h.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("Recent(10) = %d entries, want 1 (debug gated out)", len(recent))
	}
	if recent[0].Message != "shown" {
		t.Errorf("retained message = %q, want shown", recent[0].Message)
	}
}

// TestCapacityClamp verifies NewHandler clamps a sub-1 capacity to 1
// instead of building a zero-length buffer (which would panic on push).
func TestCapacityClamp(t *testing.T) {
	h := NewHandler(discarding{}, 0)
	logger := slog.New(h)
	logger.Info("a")
	logger.Info("b")
	recent := h.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("Recent(10) = %d entries, want 1 (clamped capacity)", len(recent))
	}
	if recent[0].Message != "b" {
		t.Errorf("retained message = %q, want b (newest)", recent[0].Message)
	}
}

// testLogValuer is a slog.LogValuer used to pin formatAttr's default branch.
type testLogValuer struct{ s string }

func (v testLogValuer) LogValue() slog.Value { return slog.StringValue(v.s) }

// TestFormatAttrKinds pins the value renderer across slog kinds: strings
// raw, numerics/bools via strconv, durations and times via their String /
// RFC3339 forms, and the default branch (KindAny incl. LogValuer) via
// slog.Value.String() — which does NOT resolve the LogValuer.
func TestFormatAttrKinds(t *testing.T) {
	utc := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		v    slog.Value
		want string
	}{
		{"string", slog.StringValue("abc"), "abc"},
		{"bool", slog.BoolValue(true), "true"},
		{"int64", slog.Int64Value(-7), "-7"},
		{"uint64", slog.Uint64Value(42), "42"},
		{"float64", slog.Float64Value(1.5), "1.5"},
		{"duration", slog.DurationValue(1500 * time.Millisecond), "1.5s"},
		{"time", slog.TimeValue(utc), "2026-08-17T12:00:00Z"},
		{"logvaluer", slog.AnyValue(testLogValuer{s: "resolved"}), "{resolved}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAttr(tc.v); got != tc.want {
				t.Errorf("formatAttr(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestRecentNegativeGuard is the S9 regression: Recent(-1) (a negative
// count) must return empty instead of panicking in make with a negative
// capacity. Zero behaves the same; positive counts still work.
func TestRecentNegativeGuard(t *testing.T) {
	h := NewHandler(discarding{}, 3)
	slog.New(h).Info("msg")
	if got := h.Recent(-1); len(got) != 0 {
		t.Errorf("Recent(-1) = %d entries, want 0 (S9 panic regression)", len(got))
	}
	if got := h.Recent(0); len(got) != 0 {
		t.Errorf("Recent(0) = %d entries, want 0", len(got))
	}
	if got := h.Recent(1); len(got) != 1 || got[0].Message != "msg" {
		t.Errorf("Recent(1) = %+v, want the single retained entry", got)
	}
}

// TestHandleErrorStillRetains verifies the push-before-forward order: when
// the wrapped sink's Handle returns an error, the error propagates to the
// caller but the record is still retained in the ring.
func TestHandleErrorStillRetains(t *testing.T) {
	h := NewHandler(errorSink{}, 5)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "doomed", 0)
	err := h.Handle(context.Background(), rec)
	if err == nil {
		t.Fatal("Handle returned nil, want the wrapped sink's error")
	}
	recent := h.Recent(1)
	if len(recent) != 1 || recent[0].Message != "doomed" {
		t.Errorf("ring entry missing after forward error: %+v", recent)
	}
}
