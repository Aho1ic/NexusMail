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

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Frame-Options", "DENY")
		// frame-ancestors 'none' is the modern equivalent of X-Frame-Options DENY
		// and is the only directive that actually stops the SPA from being iframed
		// by a hostile site; without it the cookie+CSRF auth model would let
		// clickjacking drive state-changing endpoints.
		c.Header("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data: http: https:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-src 'self'; frame-ancestors 'none'")
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

func (s *Server) rateLimitLogin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.allowAttempt("login:"+c.ClientIP(), loginRateLimit) {
			fail(c, 429, "rate_limited", "too many login attempts", nil)
			c.Abort()
			return
		}
		c.Next()
	}
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
