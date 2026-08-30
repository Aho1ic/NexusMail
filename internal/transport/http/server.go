package http

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"
	imapprovider "nexusmail/internal/provider/imap"
	"nexusmail/internal/provider/oauth"
	accountservice "nexusmail/internal/service/account"
	draftservice "nexusmail/internal/service/draft"
	messageservice "nexusmail/internal/service/message"
	sendservice "nexusmail/internal/service/send"
	sessionservice "nexusmail/internal/service/session"
	"nexusmail/internal/transport/http/static"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

type Hub interface {
	Serve(context.Context, *websocket.Conn)
}

type Server struct {
	cfg       config.Config
	repo      ports.Repository
	blobs     ports.BlobStore
	accounts  *accountservice.Service
	messages  *messageservice.Service
	drafts    *draftservice.Service
	sessions  *sessionservice.Service
	oauth     *oauth.Manager
	sync      *imapprovider.Supervisor
	sender    *sendservice.Worker
	hub       Hub
	appCtx    context.Context
	router    *gin.Engine
	rateMu    sync.Mutex
	rate      map[string][]time.Time
	rateSwept time.Time
}

func New(cfg config.Config, repo ports.Repository, blobs ports.BlobStore, accounts *accountservice.Service, messages *messageservice.Service, drafts *draftservice.Service, sessions *sessionservice.Service, oauthManager *oauth.Manager, syncer *imapprovider.Supervisor, sender *sendservice.Worker, hub Hub, appCtx context.Context) *Server {
	s := &Server{cfg: cfg, repo: repo, blobs: blobs, accounts: accounts, messages: messages, drafts: drafts, sessions: sessions, oauth: oauthManager, sync: syncer, sender: sender, hub: hub, appCtx: appCtx, rate: make(map[string][]time.Time)}
	s.router = s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	// Gin trusts every proxy by default, which makes ClientIP() report whatever
	// X-Forwarded-For says. The login throttle is keyed on that address, so an
	// attacker could get a fresh bucket per request just by varying the header —
	// and grow the bucket map without bound while doing it. Only addresses the
	// deployment actually declares are trusted.
	if err := router.SetTrustedProxies(s.cfg.TrustedProxies); err != nil {
		slog.Warn("invalid NEXUSMAIL_TRUSTED_PROXIES, falling back to no trusted proxies", "error", err)
		_ = router.SetTrustedProxies(nil)
	}
	router.Use(gin.Recovery(), requestID(), securityHeaders())
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.GET("/readyz", s.ready)
	api := router.Group("/api/v1")
	api.POST("/auth/session", s.rateLimitLogin(), s.createSession)
	api.GET("/oauth/:provider/callback", s.oauthCallback)
	protected := api.Group("")
	protected.Use(s.authenticate())
	protected.DELETE("/auth/session", s.deleteSession)
	protected.POST("/accounts", s.createAccount)
	protected.GET("/accounts", s.listAccounts)
	protected.GET("/accounts/:id/mailboxes", s.listMailboxes)
	protected.GET("/messages", s.listMessages)
	protected.POST("/messages/mark-read", s.markMessagesRead)
	protected.GET("/messages/:id", s.getMessage)
	protected.PATCH("/messages/:id", s.patchMessage)
	protected.GET("/messages/:id/attachments/:attachment_id", s.downloadAttachment)
	protected.GET("/drafts", s.listDrafts)
	protected.POST("/drafts", s.createDraft)
	protected.GET("/drafts/:id", s.getDraft)
	protected.PATCH("/drafts/:id", s.updateDraft)
	protected.DELETE("/drafts/:id", s.deleteDraft)
	protected.POST("/drafts/:id/attachments", s.addDraftAttachment)
	protected.DELETE("/drafts/:id/attachments/:attachment_id", s.deleteDraftAttachment)
	protected.POST("/drafts/:id/send", s.sendDraft)
	protected.POST("/drafts/:id/retry", s.sendDraft)
	protected.GET("/ws", s.websocket)
	s.mountSPA(router)
	return router
}

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

func (s *Server) listMessages(c *gin.Context) {
	filter := ports.MessageFilter{Folder: c.Query("folder"), Query: strings.TrimSpace(c.Query("query")), Cursor: c.Query("cursor")}
	filter.Limit, _ = strconv.Atoi(c.Query("limit"))
	if value, ok := optionalInt64(c, "account_id"); ok {
		filter.AccountID = value
	}
	if value, ok := optionalInt64(c, "mailbox_id"); ok {
		filter.MailboxID = value
		_ = s.sync.RequestMailbox(c.Request.Context(), *value)
	}
	if raw := c.Query("is_read"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			fail(c, 400, "invalid_filter", "is_read must be true or false", nil)
			return
		}
		filter.IsRead = &value
	}
	page, err := s.messages.List(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, page)
}

// markMessagesRead marks the current view read. The scope is expressed with the
// same query parameters as the feed so "everything I can see" cannot drift
// between the list and the button acting on it.
func (s *Server) markMessagesRead(c *gin.Context) {
	filter := ports.MessageFilter{Folder: c.Query("folder"), Query: strings.TrimSpace(c.Query("query"))}
	if value, ok := optionalInt64(c, "account_id"); ok {
		filter.AccountID = value
	}
	if value, ok := optionalInt64(c, "mailbox_id"); ok {
		filter.MailboxID = value
	}
	result, err := s.messages.MarkRead(c.Request.Context(), filter)
	if err != nil {
		// A partial success still changed state on the provider and locally, so it
		// is reported as success with a flag rather than thrown away as an error.
		if result.Updated == 0 {
			writeError(c, err)
			return
		}
		c.JSON(200, gin.H{"updated": result.Updated, "capped": result.Capped, "partial": true})
		return
	}
	c.JSON(200, gin.H{"updated": result.Updated, "capped": result.Capped})
}

func (s *Server) getMessage(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	message, attachments, err := s.messages.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if message.BodyState != "ready" {
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// gin.Recovery only covers the request goroutine, not this
					// background fetch. A panic in MIME parsing or in the FTS5
					// trigger path here would crash the whole process.
					done <- fmt.Errorf("body fetch panicked: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(s.appCtx, 30*time.Second)
			defer cancel()
			done <- s.sync.FetchBody(ctx, id)
		}()
		select {
		case err := <-done:
			if err == nil {
				message, attachments, _ = s.messages.Get(c.Request.Context(), id)
			} else {
				status = http.StatusAccepted
			}
		case <-time.After(3 * time.Second):
			status = http.StatusAccepted
		case <-c.Request.Context().Done():
			return
		}
	}
	body := gin.H{"message": message, "attachments": attachments}
	// The code is derived on read rather than stored: it is cheap to recompute and
	// a column would need a migration the runner cannot apply. Only the detail
	// endpoint pays for it — the feed would run this over a whole page of bodies.
	if code, ok := mailparser.DetectOTP(message.Subject, message.BodyText, message.BodyHTML); ok {
		body["otp_code"] = code
	}
	c.JSON(status, body)
}

func (s *Server) patchMessage(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var input struct {
		IsRead    *bool `json:"is_read"`
		IsStarred *bool `json:"is_starred"`
		Archive   bool  `json:"archive"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	message, err := s.messages.Patch(c.Request.Context(), id, ports.MessagePatch{IsRead: input.IsRead, IsStarred: input.IsStarred}, input.Archive)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, message)
}

func (s *Server) downloadAttachment(c *gin.Context) {
	messageID, ok := idParam(c, "id")
	if !ok {
		return
	}
	attachmentID, ok := idParam(c, "attachment_id")
	if !ok {
		return
	}
	blob, attachment, err := s.sync.FetchAttachment(c.Request.Context(), messageID, attachmentID)
	if err != nil {
		writeError(c, err)
		return
	}
	reader, err := s.blobs.Open(c.Request.Context(), blob)
	if err != nil {
		writeError(c, err)
		return
	}
	defer reader.Close()
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.FormatInt(blob.SizeBytes, 10))
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(attachment.Filename)}))
	_, _ = io.Copy(c.Writer, reader)
}

func (s *Server) listDrafts(c *gin.Context) {
	items, err := s.drafts.List(c.Request.Context(), c.Query("status"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items})
}
func (s *Server) getDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	draft, attachments, err := s.drafts.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"draft": draft, "attachments": attachments})
}
func (s *Server) createDraft(c *gin.Context) {
	var input draftservice.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	draft, err := s.drafts.Create(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(201, draft)
}
func (s *Server) updateDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(strings.Trim(c.GetHeader("If-Match"), `"`), 10, 64)
	if err != nil {
		fail(c, 428, "revision_required", "If-Match revision is required", nil)
		return
	}
	var input draftservice.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	draft, err := s.drafts.Update(c.Request.Context(), id, revision, input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", fmt.Sprintf(`"%d"`, draft.Revision))
	c.JSON(200, draft)
}
func (s *Server) deleteDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := s.drafts.Delete(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(204)
}

func (s *Server) addDraftAttachment(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxOutboundBytes)
	file, err := c.FormFile("file")
	if err != nil {
		fail(c, 400, "invalid_attachment", err.Error(), nil)
		return
	}
	opened, err := file.Open()
	if err != nil {
		writeError(c, err)
		return
	}
	defer opened.Close()
	blob, err := s.blobs.Put(c.Request.Context(), opened, "durable")
	if err != nil {
		writeError(c, err)
		return
	}
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	attachment := domain.DraftAttachment{DraftID: id, BlobID: blob.ID, Filename: filepath.Base(file.Filename), ContentType: contentType, SizeBytes: blob.SizeBytes, CreatedAt: time.Now().UnixMilli()}
	if err := s.repo.AddDraftAttachment(c.Request.Context(), &attachment); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(201, attachment)
}
func (s *Server) deleteDraftAttachment(c *gin.Context) {
	draftID, ok := idParam(c, "id")
	if !ok {
		return
	}
	attachmentID, ok := idParam(c, "attachment_id")
	if !ok {
		return
	}
	if err := s.repo.DeleteDraftAttachment(c.Request.Context(), draftID, attachmentID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(204)
}
func (s *Server) sendDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := s.sender.Queue(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(202, gin.H{"id": id, "status": "queued"})
}

func (s *Server) websocket(c *gin.Context) {
	conn, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	s.hub.Serve(c.Request.Context(), conn)
}
func (s *Server) ready(c *gin.Context) {
	if err := s.repo.Ping(c.Request.Context()); err != nil {
		c.JSON(503, gin.H{"status": "not_ready"})
		return
	}
	c.JSON(200, gin.H{"status": "ready"})
}

func (s *Server) mountSPA(router *gin.Engine) {
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		return
	}
	files := http.FileServer(http.FS(root))
	router.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			fail(c, 404, "not_found", "route not found", nil)
			return
		}
		path := strings.TrimPrefix(filepath.Clean(c.Request.URL.Path), "/")
		if path != "." {
			if _, err := fs.Stat(root, path); err == nil {
				files.ServeHTTP(c.Writer, c.Request)
				return
			}
		}
		index, err := fs.ReadFile(root, "index.html")
		if err != nil {
			c.Status(404)
			return
		}
		c.Data(200, "text/html; charset=utf-8", index)
	})
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
	return err == nil && subtle.ConstantTimeCompare([]byte(strings.ToLower(actual.Host)), []byte(strings.ToLower(expected.Host))) == 1 && actual.Scheme == expected.Scheme
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
func idParam(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		fail(c, 400, "invalid_id", name+" must be a positive integer", nil)
		return 0, false
	}
	return id, true
}
func optionalInt64(c *gin.Context, key string) (*int64, bool) {
	raw := c.Query(key)
	if raw == "" {
		return nil, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		fail(c, 400, "invalid_filter", key+" must be a positive integer", nil)
		return nil, false
	}
	return &value, true
}
func fail(c *gin.Context, status int, code, message string, details any) {
	requestID, _ := c.Get("request_id")
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "request_id": requestID, "details": details}})
}

// writeError maps a failure to a status from the class its producer declared, via
// the ports sentinels. It used to sniff substrings — "invalid" or "required" meant
// 400, "offline" meant 503 — which failed in both directions: only the 500 branch
// redacts, so any internal error whose text happened to contain one of those words
// was echoed to the client verbatim, and any deliberate 400 whose wording drifted
// silently became a redacted 500.
//
// Anything unclassified is an internal failure by definition: it gets 500 and its
// text is dropped rather than being guessed at.
func writeError(c *gin.Context, err error) {
	status, code := 500, "internal_error"
	switch {
	case errors.Is(err, ports.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, ports.ErrConflict):
		status, code = 409, "conflict"
	case errors.Is(err, ports.ErrInvalidInput):
		status, code = 400, "invalid_request"
	case errors.Is(err, ports.ErrUnavailable):
		status, code = 503, "provider_unavailable"
	}
	message := err.Error()
	if status == 500 {
		slog.Error("request failed", "request_id", c.GetString("request_id"), "path", c.FullPath(), "error", err)
		message = "internal server error"
	}
	fail(c, status, code, message, nil)
}
