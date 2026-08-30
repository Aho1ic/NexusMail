package http

import (
	"context"
	"log/slog"
	"net/http"
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
