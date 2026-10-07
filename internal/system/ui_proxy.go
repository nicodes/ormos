//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type previewRoute struct {
	cancel                context.CancelFunc
	localPort, publicPort int
	server                *http.Server
	session               *serveSession
}

// Reuse copy buffers across app assets and routes. Idle retention is capped at
// 512 KiB; concurrent responses beyond that still get their own working buffer.
type previewBufferPool chan []byte

func (pool previewBufferPool) Get() []byte {
	select {
	case buffer := <-pool:
		return buffer
	default:
		return make([]byte, 32<<10)
	}
}

func (pool previewBufferPool) Put(buffer []byte) {
	if len(buffer) != 32<<10 || cap(buffer) != 32<<10 {
		return
	}
	select {
	case pool <- buffer:
	default:
	}
}

func (r *previewRoute) close() {
	if r.cancel != nil {
		r.cancel()
	}
	if r.session != nil {
		r.session.stop()
	}
	if r.server != nil {
		_ = r.server.Close()
	}
}

// The destination is fixed when the listener is created. Incoming URLs,
// headers and CONNECT requests can never select a different upstream.
func (s *previewServe) proxyHandler(ctx context.Context, port int, scheme, authority string) http.Handler {
	upstream := &url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", strconv.Itoa(port))}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialLoopback(ctx, port)
		},
		MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	context.AfterFunc(ctx, transport.CloseIdleConnections)
	origin := scheme + "://" + authority
	proxy := &httputil.ReverseProxy{
		Transport:  transport,
		BufferPool: s.buffers,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			// Rewrite only a legitimate same-preview origin. Never bless a foreign
			// browser origin by translating it into localhost.
			if p.In.Header.Get("Origin") == origin {
				p.Out.Header.Set("Origin", upstream.String())
			}
			p.Out.Header.Del("Forwarded")
			p.Out.Header.Del("X-Forwarded-Host")
			p.Out.Header.Del("X-Forwarded-Proto")
		},
		ModifyResponse: func(res *http.Response) error {
			// Keep local absolute redirects on the public preview origin. Relative
			// redirects, cookies, embedding restrictions and response bodies stay intact.
			if value := res.Header.Get("Location"); value != "" {
				location, err := url.Parse(value)
				if err == nil && location.User == nil && (location.Scheme == "http" || location.Scheme == "") &&
					(location.Hostname() == "localhost" || location.Hostname() == "127.0.0.1" || location.Hostname() == "::1") && location.Port() == strconv.Itoa(port) {
					location.Scheme, location.Host = scheme, authority
					res.Header.Set("Location", location.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "App unavailable. Start your app, then refresh.", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, authority) {
			http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		if r.Method == http.MethodConnect {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if supplied := r.Header.Get("Origin"); supplied != "" && supplied != origin &&
			(r.Method != http.MethodGet && r.Method != http.MethodHead || strings.EqualFold(r.Header.Get("Upgrade"), "websocket")) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		pol, err := uiLoadPolicy()
		allowed, _ := pol.proxyAllowed(port)
		if (err != nil && !errors.Is(err, os.ErrNotExist)) || !allowed {
			http.Error(w, "Local policy does not allow opening this port.", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// Both addresses are literal loopback destinations; no DNS or incoming Host
// header can influence the upstream. Vite may bind localhost on IPv6 only.
func dialLoopback(ctx context.Context, port int) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		return conn, nil
	}
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
}
