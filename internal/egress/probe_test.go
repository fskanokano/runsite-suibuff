package egress

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// traceServer serves a Cloudflare-trace-style body at any path so the
// probe can be pointed at it via ProbeURL.
func traceServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestProbeParsesTrace guards the core contract: a successful GET parses
// the ip= and loc= lines into the result, and the request goes through the
// given dialer.
func TestProbeParsesTrace(t *testing.T) {
	const body = "fl=7f0\nh=www.cloudflare.com\nip=203.0.113.7\nloc=JP\ntls=ON\n"
	ts := traceServer(t, body, http.StatusOK)

	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	var dialed atomic.Int64
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Add(1)
		return DirectDialer(5*time.Second)(ctx, network, addr)
	}

	res := Probe(context.Background(), dialer, 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "JP" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / JP", res.IP, res.Country)
	}
	if dialed.Load() == 0 {
		t.Error("probe did not dial through the provided dialer")
	}
}

// TestProbeMissingTraceFields guards fail-open parsing: a 200 body without
// loc (or without either field) must not be an error; missing fields stay
// empty so the caller can report "unavailable".
func TestProbeMissingTraceFields(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / empty", res.IP, res.Country)
	}
}

// TestProbeErrorStatus guards non-200 handling: a trace endpoint that
// answers with an error status is a probe failure, not a parsed result.
func TestProbeErrorStatus(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\nloc=JP\n", http.StatusInternalServerError)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err == nil {
		t.Fatal("Probe succeeded against a 500 response")
	}
	if !strings.Contains(res.Err.Error(), "500") {
		t.Errorf("error %q does not mention the status", res.Err)
	}
}

// TestProbeDialError guards fail-open on an unreachable path: the dialer
// error is returned as Err rather than hanging or panicking.
func TestProbeDialError(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	boom := errors.New("dial refused")
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, boom
	}
	res := Probe(context.Background(), dialer, 5*time.Second)
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want the dialer error", res.Err)
	}
}

// TestProbeTimeout guards the timeout bound: a dialer that never answers
// must yield an Err within the probe timeout instead of blocking forever.
func TestProbeTimeout(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	// Dialer that ignores ctx and never completes; the client timeout is
	// what must bound the probe.
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {}
	}
	start := time.Now()
	res := Probe(context.Background(), dialer, 50*time.Millisecond)
	if res.Err == nil {
		t.Fatal("blocked dial did not time out")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %v, want bounded by the 50ms timeout", elapsed)
	}
}

// TestSocks5Dialer guards the SOCKS5 dialer builder: both a bare host:port
// and a socks5:// URL normalize to a usable dialer, and invalid input is
// rejected instead of returning a broken dialer.
func TestSocks5Dialer(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:9050", "socks5://127.0.0.1:9050", "socks5://user:pass@127.0.0.1:9050"} {
		d, err := Socks5Dialer(raw)
		if err != nil {
			t.Errorf("Socks5Dialer(%q): %v", raw, err)
			continue
		}
		if d == nil {
			t.Errorf("Socks5Dialer(%q) returned a nil dialer", raw)
		}
	}
	for _, bad := range []string{"", "socks5://", "://nope"} {
		if _, err := Socks5Dialer(bad); err == nil {
			t.Errorf("Socks5Dialer(%q) succeeded, want error", bad)
		}
	}
}

// TestSocks5DialerRedactsCredentials guards the proxy parse errors: a
// malformed URL carrying userinfo must never leak the password into the
// error text (probe failures are logged verbatim).
func TestSocks5DialerRedactsCredentials(t *testing.T) {
	const (
		noHost = "socks5://alice:s3cret@"
		badURL = "socks5://alice:s3cret@: bad"
		pass   = "s3cret"
	)
	for _, raw := range []string{noHost, badURL} {
		if _, err := Socks5Dialer(raw); err == nil {
			t.Errorf("Socks5Dialer(%q) succeeded, want error", raw)
			continue
		} else if strings.Contains(err.Error(), pass) {
			t.Errorf("Socks5Dialer(%q) error %q leaks the password", raw, err)
		}
	}
}

// TestSocks5DialerContextTimeout verifies that the dialer returned by Socks5Dialer
// respects context deadline / cancellation.
func TestSocks5DialerContextTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 1024)
				// Read until client closes or EOF
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	dialer, err := Socks5Dialer("socks5://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = dialer(ctx, "tcp", "203.0.113.1:80")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("dial succeeded against unresponsive SOCKS5 server")
	}
	if elapsed > time.Second {
		t.Errorf("dial took %v, want fast timeout bounded by 50ms context", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("expected deadline exceeded error, got: %v", err)
	}
}

// poll waits up to timeout for cond to hold, failing the test otherwise.
func poll(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within " + timeout.String())
}

// socks5TestServer is a minimal RFC 1928 SOCKS5 server for dial-through
// tests. It supports optional username/password auth (RFC 1929) and
// CONNECT tunneling to whatever address the client requests.
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
		// Offer only username/password (RFC 1929).
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
			_, _ = c.Write([]byte{1, 1}) // auth failure
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil { // auth success
			return
		}
	} else if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 3) // VER CMD RSV
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
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	// Bidirectional copy; read from br so buffered bytes are not lost.
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(up, br)
		done <- struct{}{}
	}()
	_, _ = io.Copy(c, up)
	<-done
}

// TestSocks5DialerDialThrough exercises the SOCKS5 dialer end to end against
// a real local SOCKS5 server: an authenticated URL must actually
// authenticate (regression for Audit B2 — userinfo was previously dropped,
// so the handshake failed), and a URL without credentials must be rejected
// by an auth-requiring proxy.
func TestSocks5DialerDialThrough(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.9\nloc=DE\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	t.Run("userinfo authenticates", func(t *testing.T) {
		srv := newSocks5TestServer(t, true, "alice", "s3cret")
		dialer, err := Socks5Dialer("socks5://alice:s3cret@" + srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		res := Probe(context.Background(), dialer, 5*time.Second)
		if res.Err != nil {
			t.Fatalf("probe through authenticated proxy failed: %v", res.Err)
		}
		if res.IP != "203.0.113.9" || res.Country != "DE" {
			t.Errorf("Probe = %q/%q, want 203.0.113.9/DE", res.IP, res.Country)
		}
		srv.mu.Lock()
		gotUser, gotPass, conns, fails := srv.gotUser, srv.gotPass, srv.conns, srv.authFails
		srv.mu.Unlock()
		if gotUser != "alice" || gotPass != "s3cret" {
			t.Errorf("proxy saw auth %q/%q, want alice/s3cret", gotUser, gotPass)
		}
		if conns != 1 || fails != 0 {
			t.Errorf("proxy conns=%d authFails=%d, want 1/0", conns, fails)
		}
	})

	t.Run("bare addr without creds rejected by auth proxy", func(t *testing.T) {
		srv := newSocks5TestServer(t, true, "alice", "s3cret")
		dialer, err := Socks5Dialer(srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		res := Probe(context.Background(), dialer, 5*time.Second)
		if res.Err == nil {
			t.Fatal("unauthenticated dial through an auth-requiring proxy succeeded")
		}
	})

	t.Run("no-auth proxy accepts plain dial", func(t *testing.T) {
		srv := newSocks5TestServer(t, false, "", "")
		dialer, err := Socks5Dialer("socks5://" + srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		res := Probe(context.Background(), dialer, 5*time.Second)
		if res.Err != nil {
			t.Fatalf("probe through plain proxy failed: %v", res.Err)
		}
		if res.IP != "203.0.113.9" {
			t.Errorf("IP = %q, want 203.0.113.9", res.IP)
		}
	})
}

// TestProbeAll guards probeAll: every path yields a result, paths run
// concurrently, and one failing path never aborts the others.
func TestProbeAll(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.1\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	t.Run("failure isolation", func(t *testing.T) {
		boom := errors.New("path dial refused")
		paths := []Path{
			{Key: "direct", Dialer: DirectDialer(5 * time.Second)},
			{Key: "proxy-0", Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return nil, boom
			}},
			{Key: "proxy-1", Dialer: DirectDialer(5 * time.Second)},
		}
		results := probeAll(context.Background(), paths, 5*time.Second)
		if len(results) != 3 {
			t.Fatalf("results = %d entries, want 3", len(results))
		}
		if r, ok := results["direct"]; !ok || r.Err != nil || r.IP != "203.0.113.1" {
			t.Errorf("direct result = %+v, want parsed success", r)
		}
		if r, ok := results["proxy-0"]; !ok || !errors.Is(r.Err, boom) {
			t.Errorf("proxy-0 result = %+v, want the dial error", r)
		}
		if r, ok := results["proxy-1"]; !ok || r.Err != nil {
			t.Errorf("proxy-1 result = %+v, want success despite proxy-0 failure", r)
		}
	})

	t.Run("paths run concurrently", func(t *testing.T) {
		// Both dialers block until BOTH have started; if probeAll dialed
		// serially this would deadlock and the timeout below fails the test.
		started := make(chan string, 2)
		release := make(chan struct{})
		dialer := func(name string) func(ctx context.Context, network, addr string) (net.Conn, error) {
			return func(ctx context.Context, network, addr string) (net.Conn, error) {
				started <- name
				<-release
				return nil, errors.New(name + " failed")
			}
		}
		paths := []Path{
			{Key: "a", Dialer: dialer("a")},
			{Key: "b", Dialer: dialer("b")},
		}
		go func() {
			<-started
			<-started
			close(release)
		}()
		done := make(chan map[string]Result, 1)
		go func() { done <- probeAll(context.Background(), paths, time.Second) }()
		select {
		case results := <-done:
			if len(results) != 2 {
				t.Fatalf("results = %d entries, want 2", len(results))
			}
			for _, key := range []string{"a", "b"} {
				if r, ok := results[key]; !ok || r.Err == nil {
					t.Errorf("result[%q] = %+v, want a failure result", key, r)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("probeAll did not run paths concurrently")
		}
	})
}

// TestRunLoop guards the background probing loop: it probes immediately on
// start, re-probes on the interval, caches failures fail-open, exits on ctx
// cancel, and guards against nil cache/logger and non-positive intervals
// (regression for Audit B7 — time.NewTicker panicked on interval <= 0).
func TestRunLoop(t *testing.T) {
	t.Run("immediate probe then interval", func(t *testing.T) {
		ts := traceServer(t, "ip=198.51.100.4\nloc=FR\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()

		var probes atomic.Int64
		dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
			probes.Add(1)
			return DirectDialer(5*time.Second)(ctx, network, addr)
		}
		cache := NewCache()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunLoop(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cache,
				[]Path{{Key: "direct", Dialer: dialer}}, 5*time.Second, 25*time.Millisecond)
		}()
		defer func() { cancel(); <-done }()

		// Immediate probe on start.
		poll(t, 2*time.Second, func() bool {
			r, ok := cache.Get("direct")
			return ok && r.Err == nil && r.IP == "198.51.100.4" && r.Country == "FR"
		})
		// Re-probe on the interval.
		poll(t, 2*time.Second, func() bool { return probes.Load() >= 2 })
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunLoop did not exit on ctx cancel")
		}
	})

	t.Run("failure cached fail-open", func(t *testing.T) {
		bad := traceServer(t, "nope", http.StatusInternalServerError)
		old := ProbeURL
		ProbeURL = bad.URL
		defer func() { ProbeURL = old }()

		cache := NewCache()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunLoop(ctx, slog.Default(), cache,
				[]Path{{Key: "proxy-0", Dialer: DirectDialer(5 * time.Second)}}, 5*time.Second, time.Hour)
		}()
		defer func() { cancel(); <-done }()
		poll(t, 2*time.Second, func() bool {
			r, ok := cache.Get("proxy-0")
			return ok && r.Err != nil
		})
	})

	t.Run("interval zero falls back to default", func(t *testing.T) {
		ts := traceServer(t, "ip=198.51.100.5\nloc=FR\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()

		cache := NewCache()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunLoop(ctx, slog.Default(), cache,
				[]Path{{Key: "direct", Dialer: DirectDialer(5 * time.Second)}}, 5*time.Second, 0)
		}()
		defer func() { cancel(); <-done }()
		poll(t, 2*time.Second, func() bool {
			r, ok := cache.Get("direct")
			return ok && r.Err == nil
		})
	})

	t.Run("nil logger falls back to default", func(t *testing.T) {
		ts := traceServer(t, "ip=198.51.100.6\nloc=FR\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()

		cache := NewCache()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunLoop(ctx, nil, cache,
				[]Path{{Key: "direct", Dialer: DirectDialer(5 * time.Second)}}, 5*time.Second, time.Hour)
		}()
		defer func() { cancel(); <-done }()
		poll(t, 2*time.Second, func() bool {
			r, ok := cache.Get("direct")
			return ok && r.Err == nil
		})
	})

	t.Run("nil cache panics with clear message", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("RunLoop with nil cache did not panic")
			}
			if msg := fmt.Sprint(r); !strings.Contains(msg, "nil cache") {
				t.Fatalf("panic message %q does not mention nil cache", msg)
			}
		}()
		RunLoop(context.Background(), slog.Default(), nil, nil, 5*time.Second, time.Minute)
	})
}

// TestCache guards the in-package TTL cache: get/set round-trip, expiry,
// re-Set refresh, TTL=0 semantics (always expired), and missing keys. This
// coverage previously lived only in cmd/freebuff-proxy, so the egress
// package itself counted 0 for it.
func TestCache(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		c := NewCache()
		if _, ok := c.Get("nope"); ok {
			t.Error("Get on empty cache returned ok")
		}
	})
	t.Run("set and get", func(t *testing.T) {
		c := NewCache()
		r := Result{IP: "1.2.3.4", Country: "US"}
		c.Set("direct", r)
		got, ok := c.Get("direct")
		if !ok || got.IP != r.IP || got.Country != r.Country || got.Err != nil {
			t.Errorf("Get = %+v ok=%v, want %+v", got, ok, r)
		}
	})
	t.Run("ttl expiry", func(t *testing.T) {
		c := NewCacheWithTTL(time.Minute)
		c.Set("direct", Result{IP: "1.2.3.4"})
		if _, ok := c.Get("direct"); !ok {
			t.Fatal("fresh entry not returned")
		}
		// Age the entry past the TTL deterministically (white-box: the
		// time.Since comparison is what matters, not real clock waits).
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-2 * time.Minute)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); ok {
			t.Error("expired entry still returned")
		}
	})
	t.Run("re-set refreshes ttl", func(t *testing.T) {
		c := NewCacheWithTTL(time.Minute)
		c.Set("direct", Result{IP: "1.1.1.1"})
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-59 * time.Second)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); !ok {
			t.Fatal("entry aged to 59s should still be fresh under a 60s TTL")
		}
		c.Set("direct", Result{IP: "2.2.2.2"})
		c.mu.Lock()
		refreshed := c.entries["direct"].At
		c.mu.Unlock()
		if time.Since(refreshed) > time.Second {
			t.Error("re-Set did not refresh the entry timestamp")
		}
	})
	t.Run("ttl zero expires immediately", func(t *testing.T) {
		c := NewCacheWithTTL(0)
		c.Set("direct", Result{IP: "1.2.3.4"})
		// With TTL=0 the entry expires as soon as the clock advances past
		// the Set instant (time.Since(e.At) > 0), so a Get racing the Set
		// on the same clock tick is undefined. Force the timestamp into the
		// past to pin the contract deterministically: an aged entry is
		// never returned.
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-time.Millisecond)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); ok {
			t.Error("Get with TTL=0 returned an entry whose timestamp is in the past")
		}
	})
}

// TestProbeBodyAndCancelEdges guards the remaining Probe branches: ctx
// cancellation mid-flight, opaque 200 bodies, CRLF line endings, and the
// scanner error path.
func TestProbeBodyAndCancelEdges(t *testing.T) {
	t.Run("ctx canceled mid-flight", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		accepted := make(chan struct{})
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			close(accepted)
			// Hold the connection open without responding; io.Copy ends
			// when the probe tears the connection down after cancel.
			_, _ = io.Copy(io.Discard, c)
			_ = c.Close()
		}()

		old := ProbeURL
		ProbeURL = "http://" + ln.Addr().String()
		defer func() { ProbeURL = old }()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)
		go func() { done <- Probe(ctx, DirectDialer(5*time.Second), time.Minute) }()
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("probe never dialed")
		}
		cancel()
		select {
		case res := <-done:
			if !errors.Is(res.Err, context.Canceled) {
				t.Errorf("Err = %v, want context.Canceled", res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("probe did not abort on cancel")
		}
	})

	t.Run("opaque 200 body is fail-open", func(t *testing.T) {
		ts := traceServer(t, "hello world\nnot a trace body\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err != nil {
			t.Fatalf("opaque body errored: %v", res.Err)
		}
		if res.IP != "" || res.Country != "" {
			t.Errorf("opaque body parsed %q/%q, want empty fields", res.IP, res.Country)
		}
	})

	t.Run("CRLF line endings parse", func(t *testing.T) {
		ts := traceServer(t, "ip=203.0.113.1\r\nloc=US\r\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err != nil {
			t.Fatalf("CRLF body errored: %v", res.Err)
		}
		if res.IP != "203.0.113.1" || res.Country != "US" {
			t.Errorf("CRLF body parsed %q/%q, want 203.0.113.1/US", res.IP, res.Country)
		}
	})

	t.Run("scanner error surfaces", func(t *testing.T) {
		// A line longer than bufio.Scanner's 64KB token cap makes the scan
		// fail; the probe must surface it as Err, not a partial result.
		big := strings.Repeat("x", 70*1024)
		ts := traceServer(t, "ip=203.0.113.1\n"+big+"\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err == nil {
			t.Fatal("oversized trace line did not fail the scanner")
		}
		if !strings.Contains(res.Err.Error(), "trace body") {
			t.Errorf("Err = %v, want a trace-body read error", res.Err)
		}
	})
}
