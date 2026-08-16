package httpx

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/observability"
)

// RequestID assigns every request a correlation id, echoing a client-supplied
// one only when it looks like a UUID so the header cannot be used to inject
// arbitrary text into logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if _, err := uuid.Parse(id); err != nil {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// RealIP resolves the client address, honouring X-Forwarded-For only when the
// immediate peer is a configured trusted proxy (§33).
func RealIP(trustedProxies []string) func(http.Handler) http.Handler {
	networks := parseCIDRs(trustedProxies)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := peerIP(r.RemoteAddr)
			if isTrusted(ip, networks) {
				if forwarded := clientFromForwardedFor(r.Header.Get("X-Forwarded-For")); forwarded != "" {
					ip = forwarded
				} else if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
					ip = real
				}
			}
			next.ServeHTTP(w, r.WithContext(WithClientIP(r.Context(), ip)))
		})
	}
}

// Logger attaches a request-scoped logger and emits one structured line per
// request. Bodies are never logged.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := r.Context()

			logger := base.With(
				slog.String("request_id", RequestIDFrom(ctx)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			ctx = WithLogger(ctx, logger)

			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r.WithContext(ctx))

			attrs := []any{
				slog.Int("status", recorder.status),
				slog.Int64("bytes", recorder.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("ip", ClientIPFrom(ctx)),
			}
			if p := PrincipalFrom(ctx); p != nil {
				attrs = append(attrs, slog.String("user_id", p.UserID.String()))
			}

			switch {
			case recorder.status >= 500:
				logger.Error("request", attrs...)
			case recorder.status >= 400:
				logger.Info("request", attrs...)
			default:
				logger.Info("request", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 envelope and keeps the process alive.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				LoggerFrom(r.Context()).Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
				)
				Fail(w, r, Internal(nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders applies the baseline hardening headers from §33. HSTS is only
// meaningful over TLS, so it is emitted for HTTPS requests.
func SecurityHeaders(production bool) func(http.Handler) http.Handler {
	const csp = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			h.Set("Cross-Origin-Resource-Policy", "same-site")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			if production || r.TLS != nil {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS permits the configured browser origins. The API is token-authenticated
// and never uses cookies, so credentials stay disabled.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowAll := len(allowedOrigins) == 1 && allowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				_, ok := allowed[strings.ToLower(origin)]
				if ok || allowAll {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Set("Vary", "Origin")
					h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers",
						"Authorization, Content-Type, X-Request-ID, X-Device-ID, X-Protocol-Version, X-Client-Version")
					h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
					h.Set("Access-Control-Max-Age", "600")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records per-route counters and latencies. It labels by chi's route
// pattern rather than the raw path so ids never explode cardinality.
func Metrics(m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.HTTPInFlight.Inc()
			defer m.HTTPInFlight.Dec()

			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)

			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			m.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(recorder.status)).Inc()
			m.HTTPDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
			m.HTTPResponseSize.WithLabelValues(route).Observe(float64(recorder.written))
		})
	}
}

// Timeout bounds handler execution so a stuck dependency cannot pin a worker.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"success":false,"error":{"code":"TIMEOUT","message":"Request timed out"}}`)
	}
}

// responseRecorder captures status and size, and forwards the Hijack and Flush
// interfaces that WebSocket upgrades and streaming responses depend on.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the raw connection to the caller.
//
// The WebSocket upgrade type-asserts http.Hijacker on the ResponseWriter it is
// given. Unwrap is not enough — only http.ResponseController consults it — so
// without this method every upgrade behind this middleware fails.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("httpx: the underlying ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

func parseCIDRs(entries []string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			if ip := net.ParseIP(entry); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
			continue
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func isTrusted(ip string, networks []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, network := range networks {
		if network.Contains(parsed) {
			return true
		}
	}
	return false
}

// clientFromForwardedFor takes the left-most entry, which is the address the
// edge proxy observed for the client.
func clientFromForwardedFor(header string) string {
	if header == "" {
		return ""
	}
	first, _, _ := strings.Cut(header, ",")
	first = strings.TrimSpace(first)
	if net.ParseIP(first) == nil {
		return ""
	}
	return first
}

func peerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
