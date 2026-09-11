package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider/oauth"
	accountservice "nexusmail/internal/service/account"
	draftservice "nexusmail/internal/service/draft"
	messageservice "nexusmail/internal/service/message"
	sessionservice "nexusmail/internal/service/session"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

type Hub interface {
	Serve(context.Context, *websocket.Conn)
}

// Syncer is the slice of the IMAP supervisor the transport drives. Declared here
// rather than taking *imap.Supervisor so a handler test does not have to stand up
// something that dials a real mail server.
type Syncer interface {
	StartAccount(context.Context, domain.Account)
	RequestMailbox(context.Context, int64) error
	FetchBody(context.Context, int64) error
	FetchAttachment(context.Context, int64, int64) (domain.BlobObject, domain.Attachment, error)
	StopAccount(int64)
}

// Sender queues a draft for delivery. Queue only writes the outbox row and wakes
// the worker, so nothing here reaches SMTP synchronously.
type Sender interface {
	Queue(context.Context, int64) error
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
	sync      Syncer
	sender    Sender
	hub       Hub
	appCtx    context.Context
	router    *gin.Engine
	rateMu    sync.Mutex
	rate      map[string][]time.Time
	rateSwept time.Time
}

func New(cfg config.Config, repo ports.Repository, blobs ports.BlobStore, accounts *accountservice.Service, messages *messageservice.Service, drafts *draftservice.Service, sessions *sessionservice.Service, oauthManager *oauth.Manager, syncer Syncer, sender Sender, hub Hub, appCtx context.Context) *Server {
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
	// gin.Recovery writes its panic report to gin.DefaultErrorWriter, and its dump
	// masks only Authorization: the broken-pipe branch would print the session
	// cookie, X-API-Key and X-CSRF-Token in the clear, outside the slog handler.
	// The nil writer removes that path entirely and recoveredPanic logs the panic
	// through slog with the request identity only.
	router.Use(gin.RecoveryWithWriter(nil, recoveredPanic), requestID(), s.securityHeaders())
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
	protected.DELETE("/accounts/:id", s.deleteAccount)
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

// wsSessionCheckInterval is how often an established websocket re-checks that the
// session which opened it still exists. The upgrade authenticates once and the hub
// then streams NEW_EMAIL and MESSAGE_UPDATED — otp_code included — for as long as
// the socket lives, so without a re-check a logout or an expired idle TTL leaves a
// stolen cookie receiving mail indefinitely. 30s bounds that window without
// touching the session table on every event.
const wsSessionCheckInterval = 30 * time.Second

func (s *Server) websocket(c *gin.Context) {
	conn, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	// An X-API-Key caller has no session row to revoke: the key is checked on every
	// request and outliving a session is meaningless for it. A cookie-authenticated
	// socket is the one that has to die with its session.
	if c.GetString("auth_method") == "session" {
		if token, cookieErr := c.Cookie(sessionservice.CookieName); cookieErr == nil {
			go s.watchSession(ctx, cancel, token, wsSessionCheckInterval)
		}
	}
	s.hub.Serve(ctx, conn)
}

// watchSession cancels ctx once the session behind a websocket stops validating,
// which is what makes Serve return and close the connection. It selects on the same
// ctx the handler defers cancel on, so it cannot outlive the handler.
//
// The interval is a parameter so a test can observe a tick without waiting out the
// production cadence.
func (s *Server) watchSession(ctx context.Context, cancel context.CancelFunc, token string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Alive, not Validate: the sliding variant would count this liveness check
			// as user activity and renew the idle deadline, so a socket held open by
			// script would refresh its own session forever — the exact case this
			// watcher exists to close. An error counts as a failure too; dropping the
			// socket makes the client reconnect and re-authenticate.
			if alive, err := s.sessions.Alive(ctx, token); err != nil || !alive {
				cancel()
				return
			}
		}
	}
}

// recoveredPanic reports a panic through the structured logger and answers with the
// standard error envelope. Only the request identity is logged: a dump would carry
// the session cookie, X-API-Key and X-CSRF-Token, and none of those belong in logs.
func recoveredPanic(c *gin.Context, recovered any) {
	slog.Error("request panicked",
		"request_id", c.GetString("request_id"),
		"method", c.Request.Method,
		"route", c.FullPath(),
		"panic", fmt.Sprint(recovered),
		"stack", string(debug.Stack()))
	fail(c, http.StatusInternalServerError, "internal_error", "internal server error", nil)
	c.Abort()
}

func (s *Server) ready(c *gin.Context) {
	if err := s.repo.Ping(c.Request.Context()); err != nil {
		c.JSON(503, gin.H{"status": "not_ready"})
		return
	}
	c.JSON(200, gin.H{"status": "ready"})
}
