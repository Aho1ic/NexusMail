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

type Service struct {
	repo   ports.Repository
	events ports.Publisher
	remote RemoteSyncer
	mu     sync.Mutex
	timers map[int64]*time.Timer
}

type RemoteSyncer interface {
	SyncDraft(context.Context, int64) error
	DeleteRemoteDraft(context.Context, int64) error
}

func New(repo ports.Repository, events ports.Publisher, remote RemoteSyncer) *Service {
	return &Service{repo: repo, events: events, remote: remote, timers: make(map[int64]*time.Timer)}
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
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
	}
	s.timers[id] = time.AfterFunc(5*time.Second, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = s.remote.SyncDraft(ctx, id)
		s.mu.Lock()
		delete(s.timers, id)
		s.mu.Unlock()
	})
	s.mu.Unlock()
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
