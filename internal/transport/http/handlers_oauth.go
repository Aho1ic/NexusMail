package http

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"nexusmail/internal/provider/oauth"

	"github.com/gin-gonic/gin"
)

// listOAuthClients reports one row per OAuth provider, configured or not. A
// provider missing from the response would be indistinguishable from one this
// build does not support, and the UI decides between the authorize button and the
// credential form from exactly this payload.
func (s *Server) listOAuthClients(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": s.oauthClients.List()})
}

func (s *Server) putOAuthClient(c *gin.Context) {
	var input struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	status, err := s.oauthClients.Save(c.Request.Context(), c.Param("provider"), input.ClientID, input.ClientSecret)
	if err != nil {
		writeError(c, err)
		return
	}
	// Logged without either value: knowing which provider's client changed is what
	// makes a later authorization failure explainable, and the id is enough for
	// that. The secret never reaches a log, and the id is not worth one.
	slog.Info("oauth client configured", "request_id", c.GetString("request_id"), "provider", status.Provider)
	c.JSON(http.StatusOK, status)
}

func (s *Server) deleteOAuthClient(c *gin.Context) {
	if err := s.oauthClients.Clear(c.Request.Context(), c.Param("provider")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// startOAuth issues an authorization URL for a provider and returns the state it
// is bound to.
//
// It exists alongside the OAuth branch of POST /accounts because the popup flow is
// not always available: a deployment whose public URL the provider cannot redirect
// to leaves the user holding the code in their address bar, and finishing that by
// hand needs the state. The redirect URI is returned with it so the page can tell
// the user exactly which callback the provider console has to have registered,
// which is the setup mistake that produces an opaque provider-side error.
func (s *Server) startOAuth(c *gin.Context) {
	var input struct {
		DisplayName string `json:"display_name"`
	}
	// An absent body is fine — the display name is optional — so only a malformed
	// one is refused. ShouldBindJSON reports EOF for an empty body.
	if err := c.ShouldBindJSON(&input); err != nil && !errors.Is(err, io.EOF) {
		fail(c, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	providerName := c.Param("provider")
	authURL, state, err := s.oauth.Start(providerName, input.DisplayName)
	if err != nil {
		writeOAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"authorization_url": authURL, "state": state, "redirect_uri": s.oauth.RedirectURI(providerName)})
}

// completeOAuth finishes a handshake from a code the user pasted back.
//
// The code is exchanged through the same Manager.Exchange the callback uses, so
// the PKCE verifier, the mail-scope check and the refresh-token requirement are
// identical on both paths. Unlike the callback this answers with the account,
// because the caller is the SPA itself rather than a popup that has to be told to
// close.
func (s *Server) completeOAuth(c *gin.Context) {
	var input struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	code, pastedState, err := oauth.ParseManualCode(input.Code)
	if err != nil {
		writeError(c, err)
		return
	}
	// A pasted redirect carries the state of the authorization the user actually
	// completed. It wins over the one the page held: they differ only when the page
	// has since started a second authorization, and redeeming the pasted code
	// against that newer state fails the PKCE check for no reason the user can see.
	state := input.State
	if pastedState != "" {
		state = pastedState
	}
	if state == "" {
		fail(c, http.StatusBadRequest, "invalid_request", "state is required", nil)
		return
	}
	providerName := c.Param("provider")
	email, displayName, refreshToken, err := s.oauth.Exchange(c.Request.Context(), providerName, state, code)
	if err != nil {
		// The provider and state detail in err does not belong in a response body,
		// but the cause does belong in the log: an exchange that fails against a
		// correctly configured client is a deployment problem, not a user one.
		slog.Warn("manual oauth exchange failed", "request_id", c.GetString("request_id"), "provider", providerName, "error", err)
		writeOAuthError(c, err)
		return
	}
	account, err := s.accounts.AddOAuth(c.Request.Context(), providerName, email, displayName, refreshToken)
	if err != nil {
		writeError(c, err)
		return
	}
	s.sync.StartAccount(s.appCtx, account)
	c.JSON(http.StatusCreated, account)
}

// writeOAuthError separates "the deployment has no OAuth client" from every other
// bad request. Both are 400, but only the former is fixed by configuring
// credentials, and the UI switches to the credential form on the code rather than
// on the message text.
func writeOAuthError(c *gin.Context, err error) {
	if errors.Is(err, oauth.ErrClientNotConfigured) {
		fail(c, http.StatusBadRequest, "oauth_not_configured", err.Error(), nil)
		return
	}
	writeError(c, err)
}
