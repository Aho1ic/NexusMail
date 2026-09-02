//go:build sqlite_fts5

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider/oauth"
	"nexusmail/internal/realtime"
	"nexusmail/internal/repository/sqlite"
	accountservice "nexusmail/internal/service/account"
	draftservice "nexusmail/internal/service/draft"
	messageservice "nexusmail/internal/service/message"
	sessionservice "nexusmail/internal/service/session"
	"nexusmail/internal/storage"

	"github.com/gin-gonic/gin"
)

// fakeProvider stands in for the IMAP supervisor across all three interfaces the
// stack consumes: Syncer for the transport, message.RemoteMutator for flag
// changes, draft.RemoteSyncer for the mirror. Everything else in these tests is
// the real component, so a request runs the real router, middleware, services and
// SQLite with only the mail server replaced.
type fakeProvider struct {
	mu sync.Mutex

	started      []domain.Account
	mailboxCalls []int64
	mailboxErr   error

	bodyCalls  []int64
	bodyErr    error
	bodyDelay  time.Duration
	bodyPanic  bool
	bodyEffect func(id int64)

	attachCalls [][2]int64
	attachBlob  domain.BlobObject
	attachMeta  domain.Attachment
	attachErr   error

	flagCalls    []int64
	flagErr      error
	seenBulk     []int64
	seenAccept   []int64
	seenErr      error
	archived     []int64
	archiveErr   error
	syncedDraft  []int64
	deletedRemo  []int64
	deleteRemErr error
}

func (f *fakeProvider) StartAccount(_ context.Context, account domain.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, account)
}

func (f *fakeProvider) RequestMailbox(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mailboxCalls = append(f.mailboxCalls, id)
	return f.mailboxErr
}

func (f *fakeProvider) FetchBody(ctx context.Context, id int64) error {
	f.mu.Lock()
	f.bodyCalls = append(f.bodyCalls, id)
	delay, shouldPanic, effect, err := f.bodyDelay, f.bodyPanic, f.bodyEffect, f.bodyErr
	f.mu.Unlock()
	if shouldPanic {
		panic("body fetch exploded")
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if effect != nil {
		effect(id)
	}
	return err
}

func (f *fakeProvider) FetchAttachment(_ context.Context, messageID, attachmentID int64) (domain.BlobObject, domain.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls = append(f.attachCalls, [2]int64{messageID, attachmentID})
	return f.attachBlob, f.attachMeta, f.attachErr
}

func (f *fakeProvider) SetFlags(_ context.Context, id int64, _, _ *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flagCalls = append(f.flagCalls, id)
	return f.flagErr
}

func (f *fakeProvider) SetSeenBulk(_ context.Context, ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seenBulk = append(f.seenBulk, ids...)
	if f.seenAccept != nil || f.seenErr != nil {
		return f.seenAccept, f.seenErr
	}
	return ids, nil
}

func (f *fakeProvider) Archive(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived = append(f.archived, id)
	return f.archiveErr
}

func (f *fakeProvider) SyncDraft(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncedDraft = append(f.syncedDraft, id)
	return nil
}

func (f *fakeProvider) DeleteRemoteDraft(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedRemo = append(f.deletedRemo, id)
	return f.deleteRemErr
}

func (f *fakeProvider) counts() (started, mailbox, body, attach int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.mailboxCalls), len(f.bodyCalls), len(f.attachCalls)
}

func (f *fakeProvider) set(mutate func(*fakeProvider)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

type fakeSender struct {
	mu     sync.Mutex
	queued []int64
	err    error
}

func (f *fakeSender) Queue(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.queued = append(f.queued, id)
	return nil
}

func (f *fakeSender) queuedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.queued...)
}

// harness is the whole stack behind one HTTP handler.
type harness struct {
	t        *testing.T
	server   *Server
	router   *gin.Engine
	repo     *sqlite.Store
	blobs    *storage.Store
	provider *fakeProvider
	sender   *fakeSender
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	blobs, err := storage.New(filepath.Join(dir, "blobs"), 1<<20, repo)
	if err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeProvider{}
	sender := &fakeSender{}
	hub := realtime.New()
	cfg := config.Config{PublicURL: "http://localhost:13737", APIKey: testAPIKey, MaxOutboundBytes: 4096}
	drafts := draftservice.New(repo, hub, remote)
	t.Cleanup(drafts.Close)
	server := New(cfg, repo, blobs,
		accountservice.New(repo, box),
		messageservice.New(repo, remote, hub),
		drafts,
		sessionservice.New(repo, testAPIKey, time.Hour, 24*time.Hour),
		oauth.New(cfg), remote, sender, hub, context.Background())
	return &harness{t: t, server: server, router: server.routes(), repo: repo, blobs: blobs, provider: remote, sender: sender}
}

// do issues an API-key authenticated request, which is the channel external
// clients use and the one that skips CSRF.
func (h *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return h.doRaw(request)
}

func (h *harness) doRaw(request *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	request.Header.Set("X-API-Key", testAPIKey)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	return response
}

func (h *harness) plain(method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	return recordThrough(h.router, method, path)
}

func recordThrough(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func decodeBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return out
}

// errorEnvelope is the shape every failure has to use, per the OpenAPI contract.
type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (h *harness) expectError(response *httptest.ResponseRecorder, status int, code string) errorEnvelope {
	h.t.Helper()
	if response.Code != status {
		h.t.Fatalf("status = %d, want %d (body %s)", response.Code, status, response.Body.String())
	}
	envelope := decodeBody[errorEnvelope](h.t, response)
	if envelope.Error.Code != code {
		h.t.Fatalf("error code = %q, want %q (body %s)", envelope.Error.Code, code, response.Body.String())
	}
	if envelope.Error.RequestID == "" {
		h.t.Error("error envelope carries no request_id")
	}
	if envelope.Error.Message == "" {
		h.t.Error("error envelope carries no message")
	}
	return envelope
}

func (h *harness) seedAccount() domain.Account {
	h.t.Helper()
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "mail@example.com", DisplayName: "Work", Provider: "qq", AuthType: "password",
		Username: "mail@example.com", IMAPHost: "imap.qq.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("sealed-credential"), Status: "connected", CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.CreateAccount(context.Background(), &account); err != nil {
		h.t.Fatal(err)
	}
	return account
}

func (h *harness) seedMailbox(accountID int64, name, role string) domain.Mailbox {
	h.t.Helper()
	now := time.Now().UnixMilli()
	mailbox := domain.Mailbox{AccountID: accountID, RemoteName: name, DisplayName: name, Role: role, SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now}
	if err := h.repo.UpsertMailbox(context.Background(), &mailbox); err != nil {
		h.t.Fatal(err)
	}
	boxes, err := h.repo.ListMailboxes(context.Background(), accountID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, item := range boxes {
		if item.RemoteName == name {
			return item
		}
	}
	h.t.Fatalf("mailbox %q was not stored", name)
	return domain.Mailbox{}
}

type seedOptions struct {
	subject   string
	bodyState string
	read      bool
	starred   bool
	received  int64
	bodyText  string
	bodyHTML  string
}

func (h *harness) seedMessage(accountID, mailboxID int64, uid uint32, opts seedOptions) int64 {
	h.t.Helper()
	if opts.bodyState == "" {
		opts.bodyState = "ready"
	}
	if opts.received == 0 {
		opts.received = time.Now().UnixMilli()
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", mailboxID, uid, opts.subject)))
	message := domain.Message{
		AccountID: accountID, Direction: "incoming", DedupeKey: digest[:], Subject: opts.subject,
		Sender: "Sender <sender@example.com>", Recipients: "me@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		Snippet: "snippet", BodyText: opts.bodyText, BodyHTML: opts.bodyHTML, BodyState: opts.bodyState,
		IsRead: opts.read, IsStarred: opts.starred,
		ReceivedAt: opts.received, CreatedAt: opts.received, UpdatedAt: opts.received,
	}
	ids, _, err := h.repo.BatchCreateOrUpdateMessages(context.Background(), []ports.MessageInput{{
		Message: &message, MailboxID: mailboxID, UID: uid, InternalDate: time.UnixMilli(opts.received),
	}})
	if err != nil {
		h.t.Fatal(err)
	}
	return ids[0]
}

// --- health and readiness -------------------------------------------------

func TestHealthzAndReadyz(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if response := h.plain(http.MethodGet, path); response.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.Code)
		}
	}
}

// readyz has to fail once the database is gone, or an orchestrator keeps routing
// traffic to a process that cannot answer a single query.
func TestReadyzFailsWithAClosedDatabase(t *testing.T) {
	h := newHarness(t)
	if err := h.repo.Close(); err != nil {
		t.Fatal(err)
	}
	if response := h.plain(http.MethodGet, "/readyz"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz returned %d with a closed database, want 503", response.Code)
	}
}

// --- accounts -------------------------------------------------------------

func TestCreateAccountStoresAndStartsSync(t *testing.T) {
	h := newHarness(t)
	response := h.do(http.MethodPost, "/api/v1/accounts", map[string]any{
		"provider": "qq", "email": "user@qq.com", "display_name": "Mine",
		"auth": map[string]string{"type": "password", "password": "authorization-code"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	account := decodeBody[domain.Account](t, response)
	if account.ID == 0 || account.Email != "user@qq.com" || account.Provider != "qq" {
		t.Fatalf("account = %+v", account)
	}
	// The credential must never come back on the wire.
	if strings.Contains(response.Body.String(), "authorization-code") {
		t.Fatal("the authorization code was echoed in the response")
	}
	if started, _, _, _ := h.provider.counts(); started != 1 {
		t.Fatalf("the account was stored but handed to the supervisor %d times", started)
	}
	// It is really in the database, with the credential sealed rather than stored raw.
	stored, err := h.repo.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored accounts = %d", len(stored))
	}
	if bytes.Contains(stored[0].SecretCiphertext, []byte("authorization-code")) {
		t.Fatal("the credential was persisted in the clear")
	}
}

func TestCreateAccountValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"missing provider", map[string]any{"email": "a@qq.com"}, 400, "invalid_request"},
		{"unknown provider", map[string]any{"provider": "nope", "email": "a@b.com"}, 400, "invalid_request"},
		{"malformed email", map[string]any{"provider": "qq", "email": "not-an-address", "auth": map[string]string{"password": "x"}}, 400, "invalid_request"},
		{"missing password", map[string]any{"provider": "qq", "email": "a@qq.com"}, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.expectError(h.do(http.MethodPost, "/api/v1/accounts", tc.body), tc.status, tc.code)
			if started, _, _, _ := h.provider.counts(); started != 0 {
				t.Fatal("a rejected account still started a sync")
			}
		})
	}
}

// An internal failure must not leak its text: only the four classified sentinels
// reach the client verbatim. The missing-OAuth-credentials error is unclassified, so
// it is a deployment detail the client must not be told.
func TestUnclassifiedErrorIsRedacted(t *testing.T) {
	h := newHarness(t)
	envelope := h.expectError(h.do(http.MethodPost, "/api/v1/accounts", map[string]any{"provider": "gmail"}), 500, "internal_error")
	if envelope.Error.Message != "internal server error" {
		t.Fatalf("message = %q, want the redacted text", envelope.Error.Message)
	}
}

func TestCreateAccountRejectsMalformedJSON(t *testing.T) {
	h := newHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader("{not json"))
	request.Header.Set("Content-Type", "application/json")
	h.expectError(h.doRaw(request), 400, "invalid_request")
}

// An OAuth provider answers with an authorization URL instead of an account, and
// nothing is stored until the callback returns. With no client id configured the
// attempt has to fail rather than half-create anything.
func TestCreateAccountOAuthNeedsConfiguration(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodPost, "/api/v1/accounts", map[string]any{"provider": "gmail"}), 500, "internal_error")
	if started, _, _, _ := h.provider.counts(); started != 0 {
		t.Fatal("an unconfigured OAuth attempt still started a sync")
	}
}

func TestCreateAccountOAuthReturnsAuthorizationURL(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.Google.ClientID = "client-id"
	h.server.cfg.Google.ClientSecret = "client-secret"
	h.server.oauth = oauth.New(h.server.cfg)
	response := h.do(http.MethodPost, "/api/v1/accounts", map[string]any{"provider": "gmail", "display_name": "Personal"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	payload := decodeBody[struct {
		AuthorizationURL string `json:"authorization_url"`
	}](t, response)
	if !strings.Contains(payload.AuthorizationURL, "accounts.google.com") || !strings.Contains(payload.AuthorizationURL, "code_challenge") {
		t.Fatalf("authorization_url = %q", payload.AuthorizationURL)
	}
}

func TestListAccountsHidesCredentials(t *testing.T) {
	h := newHarness(t)
	h.seedAccount()
	response := h.do(http.MethodGet, "/api/v1/accounts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	payload := decodeBody[struct{ Items []domain.Account }](t, response)
	if len(payload.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(payload.Items))
	}
	// The sealed credential and the host configuration must not be serialised.
	for _, secret := range []string{"sealed-credential", "imap.qq.com", "smtp.qq.com"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("%q was returned to the client", secret)
		}
	}
}

func TestListMailboxes(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	h.seedMailbox(account.ID, "Sent", "sent")
	h.seedMailbox(account.ID, "INBOX", "inbox")

	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/accounts/%d/mailboxes", account.ID), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	payload := decodeBody[struct{ Items []domain.Mailbox }](t, response)
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.Items))
	}
	// Inbox sorts ahead of sent regardless of insertion order.
	if payload.Items[0].Role != "inbox" {
		t.Fatalf("first mailbox role = %q, want inbox", payload.Items[0].Role)
	}
}

// An unknown account is an empty list rather than a 404: the route describes a
// collection, and the caller cannot tell the two apart anyway.
func TestListMailboxesOfAnUnknownAccountIsEmpty(t *testing.T) {
	h := newHarness(t)
	response := h.do(http.MethodGet, "/api/v1/accounts/4242/mailboxes", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if len(decodeBody[struct{ Items []domain.Mailbox }](t, response).Items) != 0 {
		t.Fatal("items should be empty")
	}
}

// Every path id goes through idParam, so anything that is not a positive integer
// is rejected before a handler can pass it to a query. An empty segment included:
// gin matches the route with an empty parameter rather than falling through.
func TestIDParamRejectsNonPositiveAndUnparseable(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"abc", "0", "-1", "99999999999999999999", "1.5", "", "%20", "1%20or%201=1"} {
		h.expectError(h.do(http.MethodGet, "/api/v1/accounts/"+id+"/mailboxes", nil), 400, "invalid_id")
	}
}

// --- OAuth callback -------------------------------------------------------

func TestOAuthCallbackReportsProviderError(t *testing.T) {
	h := newHarness(t)
	response := h.plain(http.MethodGet, "/api/v1/oauth/gmail/callback?error=access_denied")
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", response.Code)
	}
	// The user is sent back to the SPA with the reason, not shown a JSON error.
	if location := response.Header().Get("Location"); !strings.Contains(location, "oauth=error") || !strings.Contains(location, "access_denied") {
		t.Fatalf("Location = %q", location)
	}
}

func TestOAuthCallbackRejectsAnUnknownState(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.plain(http.MethodGet, "/api/v1/oauth/gmail/callback?state=forged&code=x"), 400, "oauth_failed")
}

// The callback sits outside the authenticated group on purpose — the provider
// redirects the browser to it and cannot present an API key — so it must be
// reachable without credentials while still being useless without valid state.
func TestOAuthCallbackNeedsNoCredentials(t *testing.T) {
	h := newHarness(t)
	if response := h.plain(http.MethodGet, "/api/v1/oauth/gmail/callback?error=denied"); response.Code == http.StatusUnauthorized {
		t.Fatal("the OAuth callback requires authentication, so no provider can reach it")
	}
}

// --- sessions -------------------------------------------------------------

func TestSessionLifecycle(t *testing.T) {
	h := newHarness(t)
	login := httptest.NewRecorder()
	h.router.ServeHTTP(login, loginRequest(testAPIKey, ""))
	if login.Code != http.StatusCreated {
		t.Fatalf("login = %d: %s", login.Code, login.Body.String())
	}
	var cookie *http.Cookie
	for _, item := range login.Result().Cookies() {
		if item.Name == sessionservice.CookieName {
			cookie = item
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	payload := decodeBody[struct {
		CSRFToken string `json:"csrf_token"`
		ExpiresAt int64  `json:"expires_at"`
	}](t, login)
	if payload.CSRFToken == "" || payload.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("login payload = %+v", payload)
	}

	// The cookie authenticates a read.
	read := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	read.AddCookie(cookie)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, read)
	if response.Code != http.StatusOK {
		t.Fatalf("cookie read = %d", response.Code)
	}

	// Logout revokes it, and the same cookie stops working.
	out := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	out.AddCookie(cookie)
	out.Header.Set("X-CSRF-Token", payload.CSRFToken)
	response = httptest.NewRecorder()
	h.router.ServeHTTP(response, out)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", response.Code)
	}
	for _, item := range response.Result().Cookies() {
		if item.Name == sessionservice.CookieName && item.MaxAge >= 0 {
			t.Fatalf("logout cookie MaxAge = %d, want negative", item.MaxAge)
		}
	}
	after := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	after.AddCookie(cookie)
	response = httptest.NewRecorder()
	h.router.ServeHTTP(response, after)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still works: %d", response.Code)
	}
}

func TestLoginRejectsAMissingKey(t *testing.T) {
	h := newHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	h.expectError(response, 400, "invalid_request")
}

func TestLoginRejectsAWrongKey(t *testing.T) {
	h := newHarness(t)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, loginRequest("wrong-key-but-long-enough-to-pass", ""))
	h.expectError(response, 401, "invalid_api_key")
}

// Logging out without a cookie is not an error: the client's intent is satisfied
// either way, and a 4xx would make a double logout look like a failure.
func TestLogoutWithoutACookieSucceeds(t *testing.T) {
	h := newHarness(t)
	if response := h.do(http.MethodDelete, "/api/v1/auth/session", nil); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
}
