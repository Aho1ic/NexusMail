package http

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sessionservice "nexusmail/internal/service/session"

	"github.com/gin-gonic/gin"
)

func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKey := c.GetHeader("X-API-Key"); apiKey != "" {
			if !s.sessions.CheckAPIKey(apiKey) {
				// Only wrong keys are counted, so a working integration is never
				// throttled however busy it gets, while guessing runs into the same
				// ceiling the login endpoint has. Keyed on the address rather than on
				// the key itself: keying on the guess would hand out a fresh budget per
				// attempt and put candidate secrets in the map.
				if !s.allowAttempt("apikey:"+c.ClientIP(), apiKeyRateLimit) {
					fail(c, 429, "rate_limited", "too many failed API key attempts", nil)
					c.Abort()
					return
				}
				fail(c, http.StatusUnauthorized, "unauthorized", "invalid API key", nil)
				c.Abort()
				return
			}
			c.Set("auth_method", "api_key")
			c.Next()
			return
		}
		token, err := c.Cookie(sessionservice.CookieName)
		if err != nil {
			fail(c, http.StatusUnauthorized, "unauthorized", "authentication required", nil)
			c.Abort()
			return
		}
		requireCSRF := c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions
		valid, err := s.sessions.Validate(c.Request.Context(), token, c.GetHeader("X-CSRF-Token"), requireCSRF)
		if err != nil || !valid {
			fail(c, http.StatusUnauthorized, "unauthorized", "invalid session or CSRF token", nil)
			c.Abort()
			return
		}
		if requireCSRF && !sameOrigin(c.Request, s.cfg.PublicURL) {
			fail(c, http.StatusForbidden, "origin_rejected", "request origin is not allowed", nil)
			c.Abort()
			return
		}
		c.Set("auth_method", "session")
		c.Next()
	}
}

// maxRequestIDLen bounds a client-supplied correlation id. 128 is well above any
// real one — a UUID is 36 characters, a W3C trace-id 32 — and far below the size
// at which echoing the value back into the response header, the error envelope and
// two slog attributes turns a request into a log-volume multiplier.
const maxRequestIDLen = 128

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !usableRequestID(id) {
			id = strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// usableRequestID admits only what a correlation id is for. The value is echoed
// into a response header, into the error envelope every client parses and into the
// structured log, so control characters and unbounded length are rejected rather
// than propagated: neither response splitting (net/http rewrites CR and LF on
// write) nor log injection (slog quotes control characters) is reachable through
// it, but nothing is gained by carrying them either, and a rejected value simply
// gets a generated id.
func usableRequestID(value string) bool {
	if value == "" || len(value) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// securityHeaders is a method because HSTS is only correct over TLS. The scheme in
// PublicURL is the same signal the session cookie's Secure flag is decided from, so
// both agree about what the deployment is.
func (s *Server) securityHeaders() gin.HandlerFunc {
	https := strings.HasPrefix(s.cfg.PublicURL, "https://")
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Frame-Options", "DENY")
		if https {
			// Only when the deployment is really HTTPS: a browser ignores HSTS on a
			// plain-HTTP response anyway, and if one ever reached a client over TLS
			// for a host that is served over http, it would pin that host to a scheme
			// the deployment does not answer on for a year.
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		// frame-ancestors 'none' is the modern equivalent of X-Frame-Options DENY
		// and is the only directive that actually stops the SPA from being iframed
		// by a hostile site; without it the cookie+CSRF auth model would let
		// clickjacking drive state-changing endpoints.
		//
		// connect-src is 'self' alone: ws:/wss: are scheme-only sources, which match
		// any host, and the only socket the app opens is the same-origin one in
		// useRealtime — already covered by 'self'. img-src keeps http:/https: because
		// that is the deliberate remote-image valve, not an oversight.
		c.Header("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data: http: https:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-src 'self'; frame-ancestors 'none'")
		c.Next()
	}
}

// apiPath reports whether a request targets the REST surface. The bare "/api" is
// in here on purpose: no route is registered at that exact path, the shortest one
// being /api/v1/..., so gin's trailing-slash redirect has nothing to match and the
// request would reach the SPA fallback and answer 200 with index.html — HTML where
// a client is parsing JSON.
func apiPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// cacheHeaders decides storability. The API and the shell get opposite policies
// from the hashed assets, which is the whole reason this is not one header.
//
// An API response carries exactly what a cache must not keep: message body_text
// and body_html, the otp_code derived on read, and the csrf_token in the login
// reply. With no Cache-Control and no Expires those are heuristically cacheable,
// so a browser is free to write them to disk and an intermediate cache that was
// never configured otherwise is free to store them. no-store is the only value
// that forbids both; no-cache would still allow the stored copy.
//
// Everything else defaults to no-cache — store it, but revalidate before use.
// index.html is why: its name never changes, so it is the one file that has to be
// re-fetched to learn the current hashed asset names. A shell served from cache
// after a deploy asks for assets that no longer exist, which is what "still seeing
// the old SPA after a release" looks like. The bundle also contains unhashed files
// copied through from web/public (sw.js, the icons), and those need the same
// revalidation for the same reason. Only the content-hashed files under /assets/
// may be pinned for a year, and that decision is made in mountSPA where a request
// is known to have resolved to one.
func cacheHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiPath(c.Request.URL.Path) {
			c.Header("Cache-Control", "no-store")
		} else {
			c.Header("Cache-Control", "no-cache")
		}
		c.Next()
	}
}

func sameOrigin(request *http.Request, publicURL string) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	expected, err := url.Parse(publicURL)
	if err != nil {
		return false
	}
	actual, err := url.Parse(origin)
	if err != nil || !isOriginURL(actual) || !isOriginURL(expected) {
		return false
	}
	return strings.EqualFold(actual.Scheme, expected.Scheme) &&
		subtle.ConstantTimeCompare([]byte(strings.ToLower(actual.Hostname())), []byte(strings.ToLower(expected.Hostname()))) == 1 &&
		effectiveOriginPort(actual) == effectiveOriginPort(expected)
}

// isOriginURL admits only the scheme, host and optional port that an Origin header
// is allowed to contain. url.Parse is deliberately permissive — it accepts a path,
// query and userinfo — but those are not origins and accepting them broadens the
// CSRF trust boundary beyond the browser's serialization rules.
func isOriginURL(value *url.URL) bool {
	return value != nil && value.Scheme != "" && value.Hostname() != "" && value.User == nil &&
		value.Path == "" && value.RawPath == "" && value.RawQuery == "" && value.Fragment == ""
}

// effectiveOriginPort makes https://mail.example and https://mail.example:443 the
// same origin. Browsers omit a default port when serializing Origin, while operators
// commonly include it in PublicURL; comparing URL.Host directly rejected every
// cookie-authenticated mutation in that otherwise valid deployment.
func effectiveOriginPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch value.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

const (
	rateWindow      = time.Minute
	loginRateLimit  = 5
	apiKeyRateLimit = 20
	rateSweepEvery  = 5 * time.Minute
)

// loginThrottleKey names the bucket a login attempt is charged to. Keyed on the
// address, not on the offered key: keying on the guess would hand out a fresh
// budget per attempt and put candidate secrets in the map.
func loginThrottleKey(c *gin.Context) string { return "login:" + c.ClientIP() }

// clearAttempts drops a bucket that a proven credential has made irrelevant. The
// attempts in it were the user's own typos, and leaving them behind would let a
// correct login be followed straight into a 429. Callers must not hold rateMu.
func (s *Server) clearAttempts(key string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	delete(s.rate, key)
}

// allowAttempt records one attempt against a sliding window and reports whether
// it fits under the limit. A bucket that empties is deleted rather than left
// behind: keys are caller controlled, so keeping spent buckets lets the map grow
// with every distinct address that ever probed the endpoint.
func (s *Server) allowAttempt(key string, limit int) bool {
	now := time.Now()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	s.sweepRate(now)
	recent := s.rate[key][:0]
	for _, item := range s.rate[key] {
		if now.Sub(item) < rateWindow {
			recent = append(recent, item)
		}
	}
	if len(recent) >= limit {
		s.rate[key] = recent
		return false
	}
	s.rate[key] = append(recent, now)
	return true
}

// sweepRate drops buckets nothing has touched for a window. Without it a key that
// is never retried keeps its slice forever, since expiry is only ever evaluated
// on the path that looks that key up again. Callers must hold rateMu.
//
// There is deliberately no cap on the total number of buckets. Between two sweeps
// an attacker with a wide source range — a /64 on a directly exposed IPv6 bind is
// the realistic one — can accumulate buckets of roughly a hundred bytes each, and
// that is true of every rate limiter keyed on the client address; a cap would only
// move the problem to choosing an eviction victim, and evicting the wrong bucket
// hands the attacker a reset. The sweep is what bounds this: keys nobody queries
// again are exactly what it collects.
func (s *Server) sweepRate(now time.Time) {
	if now.Sub(s.rateSwept) < rateSweepEvery {
		return
	}
	s.rateSwept = now
	for key, stamps := range s.rate {
		if len(stamps) == 0 || now.Sub(stamps[len(stamps)-1]) >= rateWindow {
			delete(s.rate, key)
		}
	}
}
