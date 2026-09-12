package http

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nexusmail/internal/provider"
	sessionservice "nexusmail/internal/service/session"

	"github.com/gin-gonic/gin"
)

func (s *Server) createSession(c *gin.Context) {
	var input struct {
		APIKey string `json:"api_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", "api_key is required", nil)
		return
	}
	throttle := loginThrottleKey(c)
	token, csrf, expires, err := s.sessions.Create(c.Request.Context(), input.APIKey)
	if err != nil {
		// Only failed attempts are counted, which is the policy the X-API-Key channel
		// in authenticate() already follows. The throttle used to run as middleware
		// ahead of this handler and counted every request, successes included, with no
		// path that ever reset a bucket: five anonymous requests a minute were enough
		// to spend the budget the real user needs and lock them out for as long as the
		// attacker kept going, no credential involved. Deciding the 429 after the key
		// is checked means the ceiling can only ever refuse a wrong key.
		if !s.allowAttempt(throttle, loginRateLimit) {
			fail(c, 429, "rate_limited", "too many login attempts", nil)
			return
		}
		fail(c, http.StatusUnauthorized, "invalid_api_key", "invalid API key", nil)
		return
	}
	s.clearAttempts(throttle)
	secure := strings.HasPrefix(s.cfg.PublicURL, "https://")
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionservice.CookieName, Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, Expires: time.UnixMilli(expires)})
	c.JSON(http.StatusCreated, gin.H{"csrf_token": csrf, "expires_at": expires})
}

func (s *Server) deleteSession(c *gin.Context) {
	if token, err := c.Cookie(sessionservice.CookieName); err == nil {
		_ = s.sessions.Delete(c.Request.Context(), token)
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionservice.CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode})
	c.Status(http.StatusNoContent)
}

func (s *Server) createAccount(c *gin.Context) {
	var input struct {
		Provider    string `json:"provider" binding:"required"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
		Auth        struct {
			Type     string `json:"type"`
			Password string `json:"password"`
		} `json:"auth"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	// The preset already carries the decision, and the account service branches on
	// the same field. Re-deriving it from a provider name list here meant a new
	// oauth2 provider silently fell through to AddPassword and answered with the
	// misleading "provider requires OAuth2" 400. Get also owns the unknown-provider
	// rejection, so that 400 has one source instead of two.
	preset, err := provider.Get(input.Provider)
	if err != nil {
		writeError(c, err)
		return
	}
	if preset.AuthType == "oauth2" {
		url, err := s.oauth.Start(input.Provider, input.DisplayName)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"authorization_url": url})
		return
	}
	account, err := s.accounts.AddPassword(c.Request.Context(), input.Provider, input.Email, input.DisplayName, input.Username, input.Auth.Password)
	if err != nil {
		writeError(c, err)
		return
	}
	s.sync.StartAccount(s.appCtx, account)
	c.JSON(http.StatusCreated, account)
}

func (s *Server) oauthCallback(c *gin.Context) {
	providerName := c.Param("provider")
	if oauthError := c.Query("error"); oauthError != "" {
		c.Redirect(http.StatusFound, "/?oauth=error&reason="+url.QueryEscape(oauthError))
		return
	}
	email, displayName, refreshToken, err := s.oauth.Exchange(c.Request.Context(), providerName, c.Query("state"), c.Query("code"))
	if err != nil {
		// Both failure paths redirect, because the browser that lands here is a
		// popup the SPA is waiting on: a JSON body would strand it on an error page
		// it cannot close itself. The reason is the machine-readable cause only —
		// err carries provider and state detail that does not belong in a URL the
		// user can see and share.
		c.Redirect(http.StatusFound, "/?oauth=error&reason=exchange_failed")
		return
	}
	account, err := s.accounts.AddOAuth(c.Request.Context(), providerName, email, displayName, refreshToken)
	if err != nil {
		// Same reasoning as the exchange failure, and the same popup waiting on it.
		// The usual cause is re-authorizing an address that is already connected,
		// which the UNIQUE (provider, email) index refuses.
		slog.Warn("oauth account rejected", "request_id", c.GetString("request_id"), "provider", providerName, "error", err)
		c.Redirect(http.StatusFound, "/?oauth=error&reason=account_rejected")
		return
	}
	s.sync.StartAccount(s.appCtx, account)
	c.Redirect(http.StatusFound, "/?oauth=success")
}

func (s *Server) listAccounts(c *gin.Context) {
	items, err := s.accounts.List(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items})
}

func (s *Server) deleteAccount(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	// Loops first, rows second. A supervisor still running for this account holds a
	// connection that keeps writing messages, mailboxes and status back, so deleting
	// the rows underneath a live runtime races it into re-creating them — or into
	// failing every write against an account that no longer exists.
	s.sync.StopAccount(id)
	if err := s.accounts.Delete(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func (s *Server) listMailboxes(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	items, err := s.repo.ListMailboxes(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items})
}
