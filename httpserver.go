package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// NewHTTPHandler builds the HTTP handler: the MCP endpoint at /mcp (behind OAuth unless
// auth is disabled), the OAuth endpoints, and a health check.
func NewHTTPHandler(s *server.MCPServer, auth *AuthServer, publicURL string, extraHosts []string) http.Handler {
	mcpHandler := server.NewStreamableHTTPServer(s,
		server.WithEndpointPath("/mcp"),
		// Keep SSE streams alive through proxies such as Cloudflare, which drop idle connections after 100s.
		server.WithHeartbeatInterval(30*time.Second),
		server.WithSessionIdleTTL(2*time.Hour),
		// Host checking happens in hostGuard below, which also understands the public hostname.
		server.WithDisableLocalhostProtection(true),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	var protected http.Handler = http.MaxBytesHandler(mcpHandler, 48<<20)
	if auth != nil {
		auth.Register(mux)
		protected = auth.Protect(protected)
	}
	mux.Handle("/mcp", protected)

	allowedHosts := extraHosts
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		allowedHosts = append(allowedHosts, u.Host)
	}
	return securityHeaders(hostGuard(allowedHosts, mux), strings.HasPrefix(publicURL, "https://"))
}

// hostGuard rejects requests for any Host other than the public hostname, configured
// extra hosts, or loopback, which blocks DNS rebinding and traffic aimed at the wrong host.
func hostGuard(allowedHosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) && !hostAllowed(r.Host, allowedHosts) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(host, a) {
			return true
		}
		// Accept the host with or without an explicit port.
		if h, _, err := net.SplitHostPort(host); err == nil && strings.EqualFold(h, a) {
			return true
		}
	}
	return false
}

func securityHeaders(next http.Handler, https bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if https {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP runs the HTTP server until ctx is cancelled.
func ServeHTTP(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// No WriteTimeout: MCP responses can be long-lived SSE streams.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
