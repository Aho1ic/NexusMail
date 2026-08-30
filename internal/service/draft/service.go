package draft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	"github.com/google/uuid"
)

type Input struct {
	AccountID       int64    `json:"account_id"`
	SourceMessageID *int64   `json:"source_message_id,omitempty"`
	To              []string `json:"to"`
	CC              []string `json:"cc"`
	BCC             []string `json:"bcc"`
	Subject         string   `json:"subject"`
	BodyText        string   `json:"body_text"`
}

// Store is the slice of persistence this service uses.
type Store interface {
	CreateDraft(context.Context, *domain.Draft) error
	ListDrafts(context.Context, string) ([]domain.Draft, error)
	GetDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	UpdateDraft(context.Context, *domain.Draft, int64) error
	DeleteDraft(context.Context, int64) error
}

// syncDebounce is how long an edit waits before it is pushed to the provider.
// Autosave fires every two seconds while the user types, and each push is an IMAP
// APPEND plus a delete of the previous copy, so the edits are collapsed instead of
// uploaded one per keystroke.
const syncDebounce = 5 * time.Second

type Service struct {
	repo   Store
	events ports.Publisher
	remote RemoteSyncer
	// syncDelay is syncDebounce outside tests; it exists so a test can assert the
	// debounce without waiting for it.
	syncDelay time.Duration
	mu        sync.Mutex
	timers    map[int64]*time.Timer
}

type RemoteSyncer interface {
	SyncDraft(context.Context, int64) error
	DeleteRemoteDraft(context.Context, int64) error
}

func New(repo Store, events ports.Publisher, remote RemoteSyncer) *Service {
	return &Service{repo: repo, events: events, remote: remote, syncDelay: syncDebounce, timers: make(map[int64]*time.Timer)}
}

// Close stops every pending push. The timers are the one piece of background work
// in this package, and without this they can fire after the supervisor they call
// into has already been stopped.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, timer := range s.timers {
		timer.Stop()
		delete(s.timers, id)
	}
}

func (s *Service) Create(ctx context.Context, input Input) (domain.Draft, error) {
	if input.AccountID <= 0 {
		return domain.Draft{}, ports.Invalidf("account_id is required")
	}
	if err := validateRecipients(input.To, input.CC, input.BCC); err != nil {
		return domain.Draft{}, err
	}
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: input.AccountID, SourceMessageID: input.SourceMessageID,
		RFCMessageID: fmt.Sprintf("<%s@nexusmail.local>", uuid.NewString()), Revision: 1,
		ToJSON: encode(input.To), CCJSON: encode(input.CC), BCCJSON: encode(input.BCC),
		Subject: strings.TrimSpace(input.Subject), BodyText: input.BodyText,
		Status: "draft", RemoteSyncState: "dirty", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.CreateDraft(ctx, &draft); err != nil {
		return domain.Draft{}, err
	}
	s.events.Publish(ports.Event{Type: "DRAFT_UPDATED", Data: map[string]any{"draft_id": draft.ID}})
	s.schedule(draft.ID)
	return draft, nil
}

func (s *Service) Update(ctx context.Context, id, revision int64, input Input) (domain.Draft, error) {
	if err := validateRecipients(input.To, input.CC, input.BCC); err != nil {
		return domain.Draft{}, err
	}
	draft, _, err := s.repo.GetDraft(ctx, id)
	if err != nil {
		return domain.Draft{}, err
	}
	draft.ToJSON, draft.CCJSON, draft.BCCJSON = encode(input.To), encode(input.CC), encode(input.BCC)
	draft.Subject, draft.BodyText, draft.UpdatedAt = strings.TrimSpace(input.Subject), input.BodyText, time.Now().UnixMilli()
	if err := s.repo.UpdateDraft(ctx, &draft, revision); err != nil {
		return domain.Draft{}, err
	}
	s.events.Publish(ports.Event{Type: "DRAFT_UPDATED", Data: map[string]any{"draft_id": id, "revision": draft.Revision}})
	s.schedule(id)
	return draft, nil
}

func (s *Service) List(ctx context.Context, status string) ([]domain.Draft, error) {
	return s.repo.ListDrafts(ctx, status)
}
func (s *Service) Get(ctx context.Context, id int64) (domain.Draft, []domain.DraftAttachment, error) {
	return s.repo.GetDraft(ctx, id)
}
func (s *Service) Delete(ctx context.Context, id int64) error {
	// Cancel any push still waiting on the debounce. It would fire after the row is
	// gone, find nothing, and have spent a provider connection to learn that.
	s.cancel(id)
	if s.remote != nil {
		// A draft the provider no longer has is the outcome the caller wanted, so
		// only a different remote failure blocks the local delete. Matched on the
		// sentinel: the previous substring check on "not found" also swallowed any
		// unrelated error whose text happened to contain those words.
		if err := s.remote.DeleteRemoteDraft(ctx, id); err != nil && !errors.Is(err, ports.ErrNotFound) {
			return err
		}
	}
	return s.repo.DeleteDraft(ctx, id)
}

func (s *Service) schedule(id int64) {
	if s.remote == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
	}
	s.timers[id] = time.AfterFunc(s.syncDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = s.remote.SyncDraft(ctx, id)
		s.mu.Lock()
		delete(s.timers, id)
		s.mu.Unlock()
	})
}

// cancel drops a pending push for one draft, if any is still waiting.
func (s *Service) cancel(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
}

func validateRecipients(groups ...[]string) error {
	for _, group := range groups {
		for _, value := range group {
			address, err := mail.ParseAddress(value)
			if err != nil || address.Address == "" {
				return ports.Invalidf("invalid recipient: %s", value)
			}
		}
	}
	return nil
}

func encode(value []string) string {
	if value == nil {
		value = []string{}
	}
	b, _ := json.Marshal(value)
	return string(b)
}
