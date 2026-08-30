package http

import (
	"net/http"
	"net/url"
	"strings"
	"time"

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
	token, csrf, expires, err := s.sessions.Create(c.Request.Context(), input.APIKey)
	if err != nil {
		fail(c, http.StatusUnauthorized, "invalid_api_key", "invalid API key", nil)
		return
	}
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
	if input.Provider == "gmail" || input.Provider == "outlook" {
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
		fail(c, 400, "oauth_failed", err.Error(), nil)
		return
	}
	account, err := s.accounts.AddOAuth(c.Request.Context(), providerName, email, displayName, refreshToken)
	if err != nil {
		writeError(c, err)
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
