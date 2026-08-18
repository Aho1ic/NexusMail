package message

import (
	"context"
	"errors"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type RemoteMutator interface {
	SetFlags(context.Context, int64, *bool, *bool) error
	Archive(context.Context, int64) error
}

type Service struct {
	repo   ports.Repository
	remote RemoteMutator
	events ports.Publisher
}

func New(repo ports.Repository, remote RemoteMutator, events ports.Publisher) *Service {
	return &Service{repo: repo, remote: remote, events: events}
}

func (s *Service) List(ctx context.Context, filter ports.MessageFilter) (ports.MessagePage, error) {
	return s.repo.ListMessages(ctx, filter)
}

func (s *Service) Get(ctx context.Context, id int64) (domain.Message, []domain.Attachment, error) {
	return s.repo.GetMessage(ctx, id)
}

func (s *Service) Patch(ctx context.Context, id int64, patch ports.MessagePatch, archive bool) (domain.Message, error) {
	if patch.IsRead == nil && patch.IsStarred == nil && !archive {
		return domain.Message{}, errors.New("empty message patch")
	}
	if s.remote != nil {
		if patch.IsRead != nil || patch.IsStarred != nil {
			if err := s.remote.SetFlags(ctx, id, patch.IsRead, patch.IsStarred); err != nil {
				return domain.Message{}, err
			}
		}
		if archive {
			if err := s.remote.Archive(ctx, id); err != nil {
				return domain.Message{}, err
			}
		}
	}
	message, err := s.repo.UpdateMessage(ctx, id, patch)
	if err == nil {
		s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{"message_id": id}})
	}
	return message, err
}
