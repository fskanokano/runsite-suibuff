// Package logring is a bounded in-memory log buffer for the dashboard's log
// viewer. It wraps the process slog handler so every record is written to the
// normal sink (stderr/log file) AND retained for the UI — no log file or
// docker access needed.
package logring

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one retained log record, pre-formatted for display.
type Entry struct {
	Time    string // RFC3339
	Level   string // slog level token ("INFO", "ERROR", ...)
	Message string
	Fields  []string // "key=value" pairs, flattened
}

// Ring is the bounded store shared by every handler clone.
type Ring struct {
	mu       sync.Mutex
	buf      []Entry
	next     int // next write position (ring)
	filled   int // entries written so far (grows to capacity)
	capacity int
}

// Handler forwards records to next while retaining the last capacity entries
// in the shared ring. WithAttrs/WithGroup return clones sharing the ring (the
// bound attrs are folded into the retained fields, mirroring telemetry's
// flat handler).
type Handler struct {
	ring   *Ring
	next   slog.Handler
	attrs  []slog.Attr
	groups []string
}

// NewHandler wraps next with a ring of the given capacity (records retained
// for the dashboard log viewer).
func NewHandler(next slog.Handler, capacity int) *Handler {
	if capacity < 1 {
		capacity = 1
	}
	return &Handler{ring: &Ring{buf: make([]Entry, capacity), capacity: capacity}, next: next}
}

// Recent returns up to n entries, newest first.
func (h *Handler) Recent(n int) []Entry {
	return h.ring.recent(n)
}

func (r *Ring) push(timeStr, level, message string, fields []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = Entry{Time: timeStr, Level: level, Message: message, Fields: fields}
	r.next = (r.next + 1) % r.capacity
	if r.filled < r.capacity {
		r.filled++
	}
}

func (r *Ring) recent(n int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 {
		// S9: a negative count must return empty, not panic in make with a
		// negative capacity. No in-repo caller passes one, but this is a
		// public API (dashboard Recent(200)).
		n = 0
	}
	if n > r.filled {
		n = r.filled
	}
	out := make([]Entry, 0, n)
	// Walk backwards from the newest entry.
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + r.capacity) % r.capacity
		out = append(out, r.buf[idx])
	}
	return out
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	prefix := strings.Join(h.groups, ".")
	fields := make([]string, 0, len(h.attrs)+4)
	for _, a := range h.attrs {
		fields = append(fields, flatten(prefix, a)...)
	}
	var flat []string
	rec.Attrs(func(a slog.Attr) bool {
		flat = append(flat, flatten(prefix, a)...)
		return true
	})
	fields = append(fields, flat...)
	h.ring.push(rec.Time.Format(time.RFC3339), rec.Level.String(), rec.Message, fields)
	return h.next.Handle(ctx, rec)
}

// WithAttrs clones the handler, folding the attrs into the retained fields
// and forwarding them to the wrapped sink (matching slog's contract).
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	c.next = h.next.WithAttrs(attrs)
	return &c
}

// WithGroup clones the handler, tracking the group for retained-field
// prefixes and forwarding it to the wrapped sink.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string{}, h.groups...), name)
	c.next = h.next.WithGroup(name)
	return &c
}

// flatten renders an attr subtree into "key=value" strings; group keys are
// dotted (group.subkey=value).
func flatten(prefix string, a slog.Attr) []string {
	if a.Value.Kind() == slog.KindGroup {
		// Recurse with the GROUP's own key as the new prefix so
		// slog.Group("http", slog.Int("status", 200)) renders
		// "http.status=200" — the child keys extend the group, they do not
		// replace it.
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		var out []string
		for _, child := range a.Value.Group() {
			out = append(out, flatten(key, child)...)
		}
		return out
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	return []string{key + "=" + formatAttr(a.Value)}
}

// formatAttr renders an attr value the way slog's text handler does: strings
// raw, everything else via the text formatter.
func formatAttr(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return v.String()
	}
}
