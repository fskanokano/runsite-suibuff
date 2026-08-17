package stealth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"reflect"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Dialer returns a DialTLSContext function for http.Transport that uses
// utls to impersonate a specific browser's TLS fingerprint (JA3).
//
// By sending a ClientHello matching a real browser (Chrome, Safari, Firefox),
// the connection is indistinguishable from a genuine browser session at the
// TLS layer — defeating JA3/JA3S fingerprinting deployed by CDN/WAF
// infrastructure.
//
// baseDial provides the underlying TCP dial (e.g. SOCKS5). When nil, a
// default net.Dialer with 30s timeout is used.
func Dialer(profile *Profile, baseDial func(ctx context.Context, network, addr string) (net.Conn, error), insecureSkipVerify bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if profile == nil {
		profile = DefaultProfile()
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		connProfile := GetProfileForConnection(profile)
		dialFN := baseDial
		if dialFN == nil {
			dialFN = (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext
		}

		rawConn, err := dialFN(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("stealth: tcp dial failed: %w", err)
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: invalid address %q: %w", addr, err)
		}

		helloID := connProfile.ClientHelloID

		uConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		}, helloID)

		if connProfile.CustomSpec != nil {
			// ApplyPreset re-greases the spec's extensions in place, writing
			// the first connection's key shares and GREASE values into the
			// extension objects. Safari17/18 share a package-level spec
			// singleton, so every connection after the first would reuse the
			// previous connection's key share (nil ECDHE key → "internal
			// error" handshake failure). utls docs: "Fields of TLSExtensions
			// that are slices/pointers are shared across different
			// connections with same ClientHelloSpec... avoid any shared
			// state." Clone per connection.
			if err := uConn.ApplyPreset(cloneSpec(connProfile.CustomSpec)); err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("stealth: apply custom spec failed: %w", err)
			}
		}

		// Materialize the preset's extensions first, then pin ALPN to
		// HTTP/1.1. The browser presets advertise "h2,http/1.1" and upstream
		// would negotiate h2 — but Go's http.Transport cannot use HTTP/2
		// over a *utls.UConn (its h2 path type-asserts the connection to
		// *tls.Conn), so it falls back to HTTP/1.x and chokes on the
		// server's h2 SETTINGS frame ("malformed HTTP response").
		//
		// BuildHandshakeState() must run BEFORE the mutation: the first
		// build applies the preset spec (clobbering uconn.Extensions), and
		// every later build re-applies the (mutated) extension list.
		// JA3 hashes extension types, not ALPN values, so the fingerprint is
		// unaffected.
		if err := uConn.BuildHandshakeState(); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: build handshake state failed: %w", err)
		}
		setALPN(uConn, []string{"http/1.1"})
		if err := uConn.BuildHandshakeState(); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: rebuild handshake state failed: %w", err)
		}

		if err := uConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: tls handshake failed: %w", err)
		}
		return uConn, nil
	}
}

// setALPN replaces (or appends) the ALPN extension on a utls UConn before
// the handshake. utls UConn exposes its extension list for mutation; the
// preset/custom-spec ALPN entry is replaced in place so no other extension
// ordering is disturbed.
func setALPN(uConn *utls.UConn, protocols []string) {
	ext := &utls.ALPNExtension{AlpnProtocols: protocols}
	for i, e := range uConn.Extensions {
		if _, ok := e.(*utls.ALPNExtension); ok {
			uConn.Extensions[i] = ext
			return
		}
	}
	uConn.Extensions = append(uConn.Extensions, ext)
}

// cloneSpec returns a deep copy of spec so utls's ApplyPreset re-grease
// cannot mutate a spec shared across connections or profiles (Safari17/18
// share one package-level singleton). utls warns that ClientHelloSpec
// extension slices/pointers are shared across connections; a reflective deep
// copy guarantees fresh extension objects per dial, covering any extension
// type (key shares, GREASE, curves, versions) without a per-type switch.
func cloneSpec(spec *utls.ClientHelloSpec) *utls.ClientHelloSpec {
	if spec == nil {
		return nil
	}
	c := *spec
	c.Extensions = make([]utls.TLSExtension, len(spec.Extensions))
	for i := range spec.Extensions {
		c.Extensions[i] = cloneExtension(spec.Extensions[i])
	}
	return &c
}

func cloneExtension(ext utls.TLSExtension) utls.TLSExtension {
	v := reflect.ValueOf(ext)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ext
	}
	dst := reflect.New(v.Elem().Type())
	deepCopyValue(dst.Elem(), v.Elem())
	return dst.Interface().(utls.TLSExtension)
}

// deepCopyValue recursively copies src into dst (both settable), duplicating
// every pointer, slice, map, and struct field so no underlying object is
// shared between the clone and the original.
func deepCopyValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.New(src.Elem().Type()))
		deepCopyValue(dst.Elem(), src.Elem())
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		out := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := range src.Len() {
			deepCopyValue(out.Index(i), src.Index(i))
		}
		dst.Set(out)
	case reflect.Map:
		if src.IsNil() {
			return
		}
		out := reflect.MakeMapWithSize(src.Type(), src.Len())
		iter := src.MapRange()
		for iter.Next() {
			kv := reflect.New(iter.Key().Type()).Elem()
			vv := reflect.New(iter.Value().Type()).Elem()
			deepCopyValue(kv, iter.Key())
			deepCopyValue(vv, iter.Value())
			out.SetMapIndex(kv, vv)
		}
		dst.Set(out)
	case reflect.Struct:
		for i := range src.NumField() {
			if dst.Field(i).CanSet() {
				deepCopyValue(dst.Field(i), src.Field(i))
			}
		}
	default:
		if dst.CanSet() {
			dst.Set(src)
		}
	}
}
