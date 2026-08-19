package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"
	providerauth "nexusmail/internal/provider/auth"
	accountservice "nexusmail/internal/service/account"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	messagecharset "github.com/emersion/go-message/charset"
	"github.com/emersion/go-sasl"
)

type TokenProvider interface {
	AccessToken(context.Context, domain.Account, string) (string, error)
}

type runtime struct {
	account domain.Account
	cancel  context.CancelFunc
	syncReq chan int64
	cmdMu   sync.Mutex
	// urgent counts foreground waiters for cmdMu. Background body prefetch
	// yields while it is non-zero so a new-mail sync never queues behind a
	// backlog of body fetches.
	urgent atomic.Int32
	client atomic.Pointer[imapclient.Client]
}

// lock claims the command connection for sync or user-facing work.
func (rt *runtime) lock() {
	rt.urgent.Add(1)
	rt.cmdMu.Lock()
	rt.urgent.Add(-1)
}

func (rt *runtime) unlock() { rt.cmdMu.Unlock() }

// lockBackground claims the command connection for opportunistic prefetch and
// steps aside whenever foreground work is waiting.
func (rt *runtime) lockBackground(ctx context.Context) bool {
	for ctx.Err() == nil {
		if rt.urgent.Load() == 0 {
			rt.cmdMu.Lock()
			if rt.urgent.Load() == 0 {
				return true
			}
			rt.cmdMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false
}

type Supervisor struct {
	repo         ports.Repository
	blobs        ports.BlobStore
	accounts     *accountservice.Service
	tokens       TokenProvider
	events       ports.Publisher
	mu           sync.RWMutex
	runtimes     map[int64]*runtime
	wg           sync.WaitGroup
	bodyQueue    chan int64
	bodySlots    chan struct{}
	bodySeen     sync.Map
	workerCancel context.CancelFunc
	// dial overrides transport establishment in tests; nil uses TLS to the account host.
	dial func(context.Context, domain.Account) (net.Conn, error)
	// dropIdleNotifications simulates providers that advertise IDLE but never
	// deliver EXISTS, so tests can exercise the polling safety net.
	dropIdleNotifications bool
}

const maxInlineDraftImportBytes = 1 << 20

// realtimePollInterval is a safety net for IMAP servers that advertise IDLE
// but delay or drop mailbox change notifications. IDLE still handles the
// normal path; this bounds the worst-case inbox discovery latency.
const realtimePollInterval = 5 * time.Second

const periodicSyncInterval = 5 * time.Minute

func NewSupervisor(repo ports.Repository, blobs ports.BlobStore, accounts *accountservice.Service, tokens TokenProvider, events ports.Publisher) *Supervisor {
	return &Supervisor{
		repo: repo, blobs: blobs, accounts: accounts, tokens: tokens, events: events,
		runtimes: make(map[int64]*runtime), bodyQueue: make(chan int64, 256), bodySlots: make(chan struct{}, 4),
	}
}

func (s *Supervisor) Start(ctx context.Context) error {
	accounts, err := s.repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		s.StartAccount(ctx, account)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.workerCancel = cancel
	s.mu.Unlock()
	for range 4 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.bodyWorker(workerCtx)
		}()
	}
	return nil
}

func (s *Supervisor) StartAccount(parent context.Context, account domain.Account) {
	s.mu.Lock()
	if _, exists := s.runtimes[account.ID]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	rt := &runtime{account: account, cancel: cancel, syncReq: make(chan int64, 8)}
	s.runtimes[account.ID] = rt
	s.mu.Unlock()
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.commandLoop(ctx, rt) }()
	go func() { defer s.wg.Done(); s.idleLoop(ctx, rt) }()
}

func (s *Supervisor) Stop() {
	s.mu.Lock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	for _, rt := range s.runtimes {
		rt.cancel()
		if client := rt.client.Load(); client != nil {
			_ = client.Close()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Supervisor) commandLoop(ctx context.Context, rt *runtime) {
	backoff := time.Second
	for ctx.Err() == nil {
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "connecting", nil)
		client, err := s.connect(ctx, rt.account, nil)
		if err != nil {
			s.setError(ctx, rt.account.ID, "backoff", err)
			if !waitBackoff(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		rt.client.Store(client)
		backoff = time.Second
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "syncing", nil)
		rt.lock()
		syncErr := s.syncAll(ctx, rt, client)
		rt.unlock()
		if syncErr != nil {
			s.setError(ctx, rt.account.ID, "backoff", syncErr)
			s.closeCommand(rt, client)
			continue
		}
		s.enqueueBodyCandidates(ctx, rt.account.ID)
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "connected", nil)
		ticker := time.NewTicker(periodicSyncInterval)
		// The command connection owns sync and is always alive, so the safety
		// net lives here rather than inside the IDLE state.
		probe := time.NewTicker(realtimePollInterval)
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				connected = false
			case <-probe.C:
				rt.lock()
				probeErr := s.probeInbox(ctx, rt, client)
				rt.unlock()
				if probeErr == nil {
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				} else {
					// A missing or misclassified inbox must not tear down the
					// connection; client.Closed() covers real transport loss.
					slog.Debug("mail inbox probe failed", "account_id", rt.account.ID, "error", probeErr)
				}
			case mailboxID := <-rt.syncReq:
				rt.lock()
				var err error
				if mailboxID == 0 {
					err = s.syncRole(ctx, client, rt.account.ID, "inbox")
				} else {
					mailbox, mailboxErr := s.repo.GetMailbox(ctx, mailboxID)
					if mailboxErr != nil {
						err = mailboxErr
					} else if mailbox.AccountID == rt.account.ID {
						err = s.syncMailbox(ctx, client, mailbox)
					}
				}
				rt.unlock()
				if err == nil {
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				}
				if err != nil {
					connected = false
				}
			case <-ticker.C:
				rt.lock()
				err := s.syncAll(ctx, rt, client)
				rt.unlock()
				if err == nil {
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				}
				if err != nil {
					connected = false
				}
			case <-client.Closed():
				connected = false
			}
		}
		ticker.Stop()
		probe.Stop()
		s.closeCommand(rt, client)
	}
}

func (s *Supervisor) idleLoop(ctx context.Context, rt *runtime) {
	backoff := time.Second
	for ctx.Err() == nil {
		updates := make(chan struct{}, 1)
		client, err := s.connect(ctx, rt.account, &imapclient.UnilateralDataHandler{Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil && !s.dropIdleNotifications {
				select {
				case updates <- struct{}{}:
				default:
				}
			}
		}})
		if err != nil {
			if !waitBackoff(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		backoff = time.Second
		inbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "inbox")
		if err != nil {
			// The command loop has not recorded mailboxes yet. This is a local
			// read, so retry quickly instead of idling for seconds.
			_ = client.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if _, err := client.Select(inbox.RemoteName, nil).Wait(); err != nil {
			_ = client.Close()
			continue
		}
		if !client.Caps().Has(goimap.CapIdle) {
			_ = client.Close()
			if !s.pollWithoutIdle(ctx, rt) {
				return
			}
			continue
		}
		idle, err := client.Idle()
		if err != nil {
			_ = client.Close()
			continue
		}
		refresh := time.NewTimer(time.Duration(20+rand.IntN(6)) * time.Minute)
		active := true
		for active {
			select {
			case <-ctx.Done():
				_ = client.Close()
				active = false
			case <-updates:
				s.requestSync(rt)
			case <-refresh.C:
				active = false
			case <-client.Closed():
				active = false
			}
		}
		if !refresh.Stop() {
			select {
			case <-refresh.C:
			default:
			}
		}
		_ = idle.Close()
		_ = idle.Wait()
		_ = client.Close()
	}
}

// probeInbox cheaply checks whether the inbox moved before paying for a sync.
// STATUS needs one round trip and leaves the selected mailbox untouched, so the
// safety net can run often without loading the provider. The caller must hold
// the command lock.
func (s *Supervisor) probeInbox(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "inbox")
	if err != nil {
		return err
	}
	status, err := client.Status(mailbox.RemoteName, &goimap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		return err
	}
	unchanged := mailbox.UIDNext != nil && status.UIDNext != 0 &&
		uint32(status.UIDNext) == *mailbox.UIDNext && status.UIDValidity == mailbox.UIDValidity
	if unchanged {
		return nil
	}
	return s.syncMailbox(ctx, client, mailbox)
}

// pollWithoutIdle drives sync signals for servers lacking IDLE. It returns
// false when the context is done, and true to re-probe capabilities.
func (s *Supervisor) pollWithoutIdle(ctx context.Context, rt *runtime) bool {
	ticker := time.NewTicker(realtimePollInterval)
	defer ticker.Stop()
	recheck := time.NewTimer(periodicSyncInterval)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			s.requestSync(rt)
		case <-recheck.C:
			return true
		}
	}
}

func (s *Supervisor) connect(ctx context.Context, account domain.Account, handler *imapclient.UnilateralDataHandler) (*imapclient.Client, error) {
	credential, err := s.accounts.Credential(account)
	if err != nil {
		return nil, err
	}
	options := &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: account.IMAPHost, MinVersion: tls.VersionTLS12},
		Dialer:    &net.Dialer{Timeout: 20 * time.Second}, UnilateralDataHandler: handler,
		WordDecoder: &mime.WordDecoder{CharsetReader: messagecharset.Reader},
	}
	address := net.JoinHostPort(account.IMAPHost, strconv.Itoa(account.IMAPPort))
	var client *imapclient.Client
	switch {
	case s.dial != nil:
		conn, dialErr := s.dial(ctx, account)
		if dialErr != nil {
			return nil, dialErr
		}
		client = imapclient.New(conn, options)
	case account.IMAPTLSMode == "starttls":
		client, err = imapclient.DialStartTLS(address, options)
	default:
		client, err = imapclient.DialTLS(address, options)
	}
	if err != nil {
		return nil, err
	}
	if account.AuthType == "oauth2" {
		token, tokenErr := s.tokens.AccessToken(ctx, account, credential.RefreshToken)
		if tokenErr != nil {
			_ = client.Close()
			return nil, tokenErr
		}
		err = client.Authenticate(&providerauth.XOAUTH2{Username: account.Username, AccessToken: token})
	} else {
		if client.Caps().Has(goimap.AuthCap("PLAIN")) {
			err = client.Authenticate(sasl.NewPlainClient("", account.Username, credential.Password))
		} else {
			err = client.Login(account.Username, credential.Password).Wait()
		}
	}
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (s *Supervisor) syncAll(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	items, err := client.List("", "*", nil).Collect()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, item := range items {
		attrs := make([]string, len(item.Attrs))
		for i, attr := range item.Attrs {
			attrs[i] = string(attr)
		}
		role, mode := provider.ClassifyMailbox(item.Mailbox, attrs)
		var delimiter *string
		if item.Delim != 0 {
			value := string(item.Delim)
			delimiter = &value
		}
		mailbox := domain.Mailbox{AccountID: rt.account.ID, RemoteName: item.Mailbox, DisplayName: item.Mailbox, Delimiter: delimiter, Role: role, SyncMode: mode, CreatedAt: now, UpdatedAt: now}
		if err := s.repo.UpsertMailbox(ctx, &mailbox); err != nil {
			return err
		}
	}
	mailboxes, err := s.repo.ListMailboxes(ctx, rt.account.ID)
	if err != nil {
		return err
	}
	for _, mailbox := range mailboxes {
		if mailbox.SyncMode == "lazy" {
			continue
		}
		if err := s.syncMailbox(ctx, client, mailbox); err != nil {
			return fmt.Errorf("sync %s: %w", mailbox.RemoteName, err)
		}
		// A full sync can span minutes on large accounts. Serve queued inbox
		// signals between mailboxes so new mail is not stuck behind it.
		if err := s.drainPending(ctx, rt, client); err != nil {
			return err
		}
	}
	return nil
}

// drainPending services queued sync requests. The caller must hold the command
// lock; each syncMailbox re-selects its own mailbox, so this is only safe
// between mailboxes, never inside one.
func (s *Supervisor) drainPending(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	for {
		select {
		case mailboxID := <-rt.syncReq:
			if mailboxID == 0 {
				if err := s.syncRole(ctx, client, rt.account.ID, "inbox"); err != nil {
					return err
				}
				continue
			}
			mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
			if err != nil {
				return err
			}
			if mailbox.AccountID != rt.account.ID {
				continue
			}
			if err := s.syncMailbox(ctx, client, mailbox); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (s *Supervisor) syncRole(ctx context.Context, client *imapclient.Client, accountID int64, role string) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, role)
	if err != nil {
		return err
	}
	return s.syncMailbox(ctx, client, mailbox)
}

func (s *Supervisor) syncMailbox(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox) error {
	selected, err := client.Select(mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return err
	}
	if mailbox.UIDValidity != 0 && mailbox.UIDValidity != selected.UIDValidity {
		if err := s.repo.ResetMailbox(ctx, mailbox.ID, selected.UIDValidity); err != nil {
			return err
		}
		mailbox.LastUID = 0
	}
	mailbox.UIDValidity = selected.UIDValidity
	criteria := &goimap.SearchCriteria{}
	if mailbox.LastUID == 0 {
		criteria.Since = time.Now().AddDate(0, 0, -30)
	} else {
		var set goimap.UIDSet
		set.AddRange(goimap.UID(mailbox.LastUID+1), 0)
		criteria.UID = []goimap.UIDSet{set}
	}
	search, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return err
	}
	uids := search.AllUIDs()
	lastUID := mailbox.LastUID
	for start := 0; start < len(uids); start += 100 {
		end := min(start+100, len(uids))
		fetchOptions := &goimap.FetchOptions{
			UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
			BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		}
		messages, err := client.Fetch(goimap.UIDSetNum(uids[start:end]...), fetchOptions).Collect()
		if err != nil {
			return err
		}
		for _, fetched := range messages {
			if mailbox.Role == "drafts" {
				var raw []byte
				if fetched.RFC822Size > 0 && fetched.RFC822Size <= maxInlineDraftImportBytes {
					section := &goimap.FetchItemBodySection{Peek: true}
					bodies, fetchErr := client.Fetch(goimap.UIDSetNum(fetched.UID), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
					if fetchErr == nil && len(bodies) > 0 {
						raw = bodies[0].FindBodySection(section)
					}
				}
				changed, draftID, err := s.storeRemoteDraft(ctx, mailbox, fetched, raw)
				if err != nil {
					return err
				}
				if changed {
					s.events.Publish(ports.Event{Type: "DRAFT_UPDATED", Data: map[string]any{"draft_id": draftID, "account_id": mailbox.AccountID, "remote": true}})
				}
				if uint32(fetched.UID) > lastUID {
					lastUID = uint32(fetched.UID)
				}
				continue
			}
			created, messageID, err := s.storeFetched(ctx, mailbox, fetched)
			if err != nil {
				return err
			}
			if created {
				s.events.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"message_id": messageID, "account_id": mailbox.AccountID, "mailbox_id": mailbox.ID}})
			}
			if uint32(fetched.UID) > lastUID {
				lastUID = uint32(fetched.UID)
			}
		}
	}
	uidNext := uint32(selected.UIDNext)
	highest := selected.HighestModSeq
	return s.repo.UpdateMailboxCursor(ctx, mailbox.ID, selected.UIDValidity, lastUID, &uidNext, &highest)
}

func (s *Supervisor) storeRemoteDraft(ctx context.Context, mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer, raw []byte) (bool, int64, error) {
	if fetched.Envelope == nil {
		return false, 0, nil
	}
	parsed := mailparser.Parsed{}
	if len(raw) > 0 {
		value, err := mailparser.Parse(bytes.NewReader(raw))
		if err != nil {
			return false, 0, err
		}
		parsed = value
	}
	rfcID := strings.TrimSpace(fetched.Envelope.MessageID)
	if rfcID == "" {
		rfcID = fmt.Sprintf("<remote-%d-%d-%d-%d@nexusmail.local>", mailbox.AccountID, mailbox.ID, mailbox.UIDValidity, fetched.UID)
	}
	to := parsed.To
	cc := parsed.CC
	bcc := parsed.BCC
	if len(to) == 0 {
		to = imapAddresses(fetched.Envelope.To)
	}
	if len(cc) == 0 {
		cc = imapAddresses(fetched.Envelope.Cc)
	}
	remoteTime := fetched.InternalDate.UnixMilli()
	if remoteTime <= 0 {
		remoteTime = time.Now().UnixMilli()
	}
	mailboxID, validity, uid := mailbox.ID, mailbox.UIDValidity, uint32(fetched.UID)
	draft := domain.Draft{
		AccountID: mailbox.AccountID, RFCMessageID: rfcID, Revision: 1,
		ToJSON: encodeMailAddresses(to), CCJSON: encodeMailAddresses(cc), BCCJSON: encodeMailAddresses(bcc),
		Subject: fetched.Envelope.Subject, BodyText: parsed.Text, Status: "draft", RemoteSyncState: "synced",
		RemoteMailboxID: &mailboxID, RemoteUIDValidity: &validity, RemoteUID: &uid, RemoteUpdatedAt: &remoteTime,
		CreatedAt: remoteTime, UpdatedAt: remoteTime,
	}
	if parsed.Subject != "" {
		draft.Subject = parsed.Subject
	}
	reconciled, changed, err := s.repo.ReconcileRemoteDraft(ctx, &draft)
	return changed, reconciled.ID, err
}

func imapAddresses(input []goimap.Address) []*mail.Address {
	result := make([]*mail.Address, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, &mail.Address{Name: value.Name, Address: value.Addr()})
		}
	}
	return result
}

func encodeMailAddresses(input []*mail.Address) string {
	values := make([]string, 0, len(input))
	for _, value := range input {
		values = append(values, value.String())
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func (s *Supervisor) storeFetched(ctx context.Context, mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer) (bool, int64, error) {
	if fetched.Envelope == nil {
		return false, 0, nil
	}
	envelope := fetched.Envelope
	rfcID := strings.TrimSpace(envelope.MessageID)
	dedupeSource := rfcID + "\x00" + strconv.FormatInt(fetched.RFC822Size, 10)
	if rfcID == "" {
		dedupeSource = fmt.Sprintf("%d:%d:%d:%d", mailbox.ID, mailbox.UIDValidity, fetched.UID, fetched.RFC822Size)
	}
	digest := sha256.Sum256([]byte(dedupeSource))
	from := addresses(envelope.From)
	to := addresses(envelope.To)
	cc := addresses(envelope.Cc)
	fromJSON, _ := json.Marshal(from)
	toJSON, _ := json.Marshal(to)
	ccJSON, _ := json.Marshal(cc)
	now := time.Now().UnixMilli()
	received := fetched.InternalDate.UnixMilli()
	if received == 0 {
		received = now
	}
	message := domain.Message{
		AccountID: mailbox.AccountID, Direction: "incoming", DedupeKey: digest[:], Subject: envelope.Subject,
		Sender: strings.Join(from, " "), Recipients: strings.Join(append(to, cc...), " "), FromJSON: string(fromJSON), ToJSON: string(toJSON), CCJSON: string(ccJSON),
		BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]", BodyState: "metadata", SizeBytes: fetched.RFC822Size,
		ReceivedAt: received, IsRead: hasFlag(fetched.Flags, goimap.FlagSeen), IsStarred: hasFlag(fetched.Flags, goimap.FlagFlagged), CreatedAt: now, UpdatedAt: now,
	}
	if rfcID != "" {
		message.RFCMessageID = &rfcID
	}
	if !envelope.Date.IsZero() {
		value := envelope.Date.UnixMilli()
		message.SentAt = &value
	}
	flagValues := make([]string, len(fetched.Flags))
	for i, flag := range fetched.Flags {
		flagValues[i] = string(flag)
	}
	created, err := s.repo.CreateOrUpdateMessage(ctx, &message, mailbox.ID, uint32(fetched.UID), flagValues, fetched.InternalDate)
	if err != nil {
		return false, 0, err
	}
	if fetched.BodyStructure != nil {
		fetched.BodyStructure.Walk(func(path []int, part goimap.BodyStructure) bool {
			single, ok := part.(*goimap.BodyStructureSinglePart)
			if !ok {
				return true
			}
			filename := single.Filename()
			disposition := single.Disposition()
			if filename == "" && disposition == nil {
				return true
			}
			dispositionValue := "attachment"
			if disposition != nil && strings.EqualFold(disposition.Value, "inline") {
				dispositionValue = "inline"
			}
			partValues := make([]string, len(path))
			for i, value := range path {
				partValues[i] = strconv.Itoa(value)
			}
			att := domain.Attachment{MessageID: message.ID, PartID: strings.Join(partValues, "."), Filename: filename, ContentType: single.MediaType(), Disposition: dispositionValue, SizeBytes: int64(single.Size), FetchState: "metadata", CreatedAt: now, UpdatedAt: now}
			if single.ID != "" {
				value := single.ID
				att.ContentID = &value
			}
			_ = s.repo.UpsertAttachment(ctx, &att)
			return true
		})
	}
	return created, message.ID, nil
}

func addresses(input []goimap.Address) []string {
	result := make([]string, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, (&mail.Address{Name: value.Name, Address: value.Addr()}).String())
		}
	}
	return result
}
func hasFlag(flags []goimap.Flag, target goimap.Flag) bool {
	for _, flag := range flags {
		if flag == target {
			return true
		}
	}
	return false
}
func (s *Supervisor) requestSync(rt *runtime) {
	select {
	case rt.syncReq <- 0:
	default:
	}
}

func (s *Supervisor) RequestMailbox(ctx context.Context, mailboxID int64) error {
	mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(mailbox.AccountID)
	if err != nil {
		return err
	}
	select {
	case rt.syncReq <- mailboxID:
	default:
	}
	return nil
}
func (s *Supervisor) closeCommand(rt *runtime, client *imapclient.Client) {
	rt.client.CompareAndSwap(client, nil)
	_ = client.Close()
}
func (s *Supervisor) setError(ctx context.Context, id int64, status string, err error) {
	value := err.Error()
	if updateErr := s.repo.UpdateAccountStatus(ctx, id, status, &value); updateErr != nil {
		slog.Error("mail account status update failed", "account_id", id, "status", status, "error", updateErr)
	}
	slog.Error("mail account sync failed", "account_id", id, "status", status, "error", value)
	s.events.Publish(ports.Event{Type: "ACCOUNT_STATUS", Data: map[string]any{"account_id": id, "status": status, "error": value}})
}
func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(delay) + 1)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// FetchBody retrieves a message body for a waiting caller and takes priority
// over background prefetch.
func (s *Supervisor) FetchBody(ctx context.Context, messageID int64) error {
	return s.fetchBody(ctx, messageID, false)
}

func (s *Supervisor) fetchBody(ctx context.Context, messageID int64, background bool) (resultErr error) {
	select {
	case s.bodySlots <- struct{}{}:
		defer func() { <-s.bodySlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	message, _, err := s.repo.GetMessage(ctx, messageID)
	if err != nil {
		return err
	}
	if message.BodyState == "ready" {
		return nil
	}
	if err := s.repo.SetMessageBodyState(ctx, messageID, "fetching"); err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = s.repo.SetMessageBodyState(context.Background(), messageID, "error")
		}
	}()
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	if background {
		if !rt.lockBackground(ctx) {
			return ctx.Err()
		}
	} else {
		rt.lock()
	}
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return err
	}
	section := &goimap.FetchItemBodySection{Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = errors.New("message body not found")
		}
		return err
	}
	body := items[0].FindBodySection(section)
	parsed, err := mailparser.Parse(bytes.NewReader(body))
	if err != nil {
		return err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(body), "cache")
	if err != nil {
		return err
	}
	return s.repo.UpdateMessageBody(ctx, messageID, parsed.Text, parsed.HTML, parsed.Snippet, &blob.ID)
}

func (s *Supervisor) enqueueBodyCandidates(ctx context.Context, accountID int64) {
	ids, err := s.repo.ListBodyCandidateIDs(ctx, accountID, maxInlineDraftImportBytes, 100)
	if err != nil {
		return
	}
	for _, id := range ids {
		if _, loaded := s.bodySeen.LoadOrStore(id, struct{}{}); loaded {
			continue
		}
		if err := s.repo.SetMessageBodyState(ctx, id, "queued"); err != nil {
			s.bodySeen.Delete(id)
			continue
		}
		select {
		case s.bodyQueue <- id:
		case <-ctx.Done():
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		default:
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		}
	}
}

func (s *Supervisor) bodyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.bodyQueue:
			fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.fetchBody(fetchCtx, id, true)
			cancel()
			s.bodySeen.Delete(id)
			if err == nil {
				s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{"message_id": id}})
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

func (s *Supervisor) FetchAttachment(ctx context.Context, messageID, attachmentID int64) (domain.BlobObject, domain.Attachment, error) {
	attachment, err := s.repo.GetAttachment(ctx, messageID, attachmentID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if attachment.BlobID != nil {
		blob, err := s.repo.GetBlob(ctx, *attachment.BlobID)
		return blob, attachment, err
	}
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	path, err := parsePartID(attachment.PartID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return domain.BlobObject{}, attachment, errors.New("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	section := &goimap.FetchItemBodySection{Part: path, Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = errors.New("attachment not found")
		}
		return domain.BlobObject{}, attachment, err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(items[0].FindBodySection(section)), "cache")
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if err := s.repo.UpdateAttachmentBlob(ctx, attachment.ID, blob.ID); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	attachment.BlobID = &blob.ID
	attachment.FetchState = "ready"
	return blob, attachment, nil
}

func (s *Supervisor) SetFlags(ctx context.Context, messageID int64, isRead, isStarred *bool) error {
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
	for flag, value := range map[goimap.Flag]*bool{goimap.FlagSeen: isRead, goimap.FlagFlagged: isStarred} {
		if value == nil {
			continue
		}
		op := goimap.StoreFlagsDel
		if *value {
			op = goimap.StoreFlagsAdd
		}
		if _, err := client.Store(uidSet, &goimap.StoreFlags{Op: op, Silent: true, Flags: []goimap.Flag{flag}}, nil).Collect(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) Archive(ctx context.Context, messageID int64) error {
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	destination, err := s.repo.GetMailboxByRole(ctx, location.Account.ID, "archive")
	if err != nil {
		return errors.New("archive mailbox is unavailable")
	}
	if destination.ID == location.Mailbox.ID {
		return nil
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
	if client.Caps().Has(goimap.CapMove) {
		data, moveErr := client.Move(uidSet, destination.RemoteName).Wait()
		if moveErr != nil {
			return moveErr
		}
		return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, firstDestinationUID(data.DestUIDs))
	}
	copyData, err := client.Copy(uidSet, destination.RemoteName).Wait()
	if err != nil {
		return err
	}
	if _, err = client.Store(uidSet, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	if client.Caps().Has(goimap.CapUIDPlus) {
		if _, err = client.UIDExpunge(uidSet).Collect(); err != nil {
			return err
		}
	}
	var destinationUID *uint32
	if copyData != nil {
		destinationUID = firstDestinationUID(copyData.DestUIDs)
	}
	return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, destinationUID)
}

func firstDestinationUID(set goimap.NumSet) *uint32 {
	uids, ok := set.(goimap.UIDSet)
	if !ok {
		return nil
	}
	values, ok := uids.Nums()
	if !ok || len(values) == 0 {
		return nil
	}
	value := uint32(values[0])
	return &value
}

func (s *Supervisor) runtime(accountID int64) (*runtime, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := s.runtimes[accountID]
	if rt == nil {
		return nil, errors.New("account runtime is not started")
	}
	return rt, nil
}
func parsePartID(value string) ([]int, error) {
	parts := strings.Split(value, ".")
	result := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, errors.New("invalid attachment part")
		}
		result[i] = n
	}
	return result, nil
}

func (s *Supervisor) SyncDraft(ctx context.Context, draftID int64) error {
	draft, attachments, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.Status != "draft" && draft.Status != "failed" && draft.Status != "unknown" {
		return nil
	}
	account, err := s.repo.GetAccount(ctx, draft.AccountID)
	if err != nil {
		return err
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return errors.New("remote drafts mailbox is unavailable")
	}
	to, err := decodeDraftAddresses(draft.ToJSON)
	if err != nil {
		return err
	}
	cc, err := decodeDraftAddresses(draft.CCJSON)
	if err != nil {
		return err
	}
	bcc, err := decodeDraftAddresses(draft.BCCJSON)
	if err != nil {
		return err
	}
	outgoingAttachments := make([]mailparser.OutgoingAttachment, 0, len(attachments))
	var closers []interface{ Close() error }
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()
	for _, attachment := range attachments {
		blob, err := s.repo.GetBlob(ctx, attachment.BlobID)
		if err != nil {
			return err
		}
		reader, err := s.blobs.Open(ctx, blob)
		if err != nil {
			return err
		}
		closers = append(closers, reader)
		outgoingAttachments = append(outgoingAttachments, mailparser.OutgoingAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Data: reader})
	}
	payload, err := mailparser.Compose(mailparser.Outgoing{
		MessageID: draft.RFCMessageID, From: mail.Address{Name: account.DisplayName, Address: account.Email},
		To: to, CC: cc, BCC: bcc, Subject: draft.Subject, BodyText: draft.BodyText, Attachments: outgoingAttachments,
	})
	if err != nil {
		return err
	}
	rt, err := s.runtime(account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	appended, err := command.Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUID != nil && draft.RemoteUIDValidity != nil && *draft.RemoteUIDValidity == appended.UIDValidity {
		if _, selectErr := client.Select(mailbox.RemoteName, nil).Wait(); selectErr == nil {
			old := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
			if _, storeErr := client.Store(old, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); storeErr == nil && client.Caps().Has(goimap.CapUIDPlus) {
				_, _ = client.UIDExpunge(old).Collect()
			}
		}
	}
	now := time.Now().UnixMilli()
	return s.repo.UpdateDraftRemote(ctx, draft.ID, mailbox.ID, appended.UIDValidity, uint32(appended.UID), now, "synced", nil)
}

func (s *Supervisor) DeleteRemoteDraft(ctx context.Context, draftID int64) error {
	draft, _, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.RemoteMailboxID == nil || draft.RemoteUID == nil {
		return nil
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return err
	}
	rt, err := s.runtime(draft.AccountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	selected, err := client.Select(mailbox.RemoteName, nil).Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUIDValidity != nil && selected.UIDValidity != *draft.RemoteUIDValidity {
		return nil
	}
	uids := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
	if _, err := client.Store(uids, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	if client.Caps().Has(goimap.CapUIDPlus) {
		_, err = client.UIDExpunge(uids).Collect()
		return err
	}
	return nil
}

func (s *Supervisor) AppendSent(ctx context.Context, accountID int64, payload []byte) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, "sent")
	if err != nil {
		return errors.New("remote sent mailbox is unavailable")
	}
	rt, err := s.runtime(accountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return errors.New("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagSeen}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	_, err = command.Wait()
	return err
}

func decodeDraftAddresses(raw string) ([]mail.Address, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	result := make([]mail.Address, 0, len(values))
	for _, value := range values {
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, err
		}
		result = append(result, *address)
	}
	return result, nil
}
