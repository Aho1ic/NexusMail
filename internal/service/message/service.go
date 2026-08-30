package message

import (
	"context"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type RemoteMutator interface {
	SetFlags(context.Context, int64, *bool, *bool) error
	SetSeenBulk(context.Context, []int64) ([]int64, error)
	Archive(context.Context, int64) error
}

// Store is the slice of persistence this service uses. It is declared here rather
// than taking ports.Repository so a test can stand up a fake with five methods.
type Store interface {
	ListMessages(context.Context, ports.MessageFilter) (ports.MessagePage, error)
	GetMessage(context.Context, int64) (domain.Message, []domain.Attachment, error)
	UpdateMessage(context.Context, int64, ports.MessagePatch) (domain.Message, error)
	UpdateMessages(context.Context, []int64, ports.MessagePatch) error
	UnreadMessageIDs(context.Context, ports.MessageFilter, int) ([]int64, error)
}

type Service struct {
	repo   Store
	remote RemoteMutator
	events ports.Publisher
}

func New(repo Store, remote RemoteMutator, events ports.Publisher) *Service {
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
		// Classified so the transport answers 400 with this text. An unclassified
		// error would be reported as a redacted 500, telling a client that sent
		// {} nothing about what was wrong with the request.
		return domain.Message{}, ports.Invalidf("empty message patch")
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

// MarkReadResult reports how much of the view was marked read. Capped is true
// when the view held more unread mail than one call is allowed to touch, so the
// caller can tell the user another pass is needed.
type MarkReadResult struct {
	Updated int  `json:"updated"`
	Capped  bool `json:"capped"`
}

// markReadLimit bounds one bulk mark-read. A view can hold years of unread mail,
// and every message becomes an IMAP STORE against the provider, so the operation
// is capped and the caller is told when the cap was reached.
const markReadLimit = 2000

// MarkRead flags every unread message in a view as read. The provider is updated
// first, exactly as Patch does, and only the messages the provider accepted are
// written locally — a row that silently diverged from the server would otherwise
// look read here and unread in every other client.
func (s *Service) MarkRead(ctx context.Context, filter ports.MessageFilter) (MarkReadResult, error) {
	ids, err := s.repo.UnreadMessageIDs(ctx, filter, markReadLimit)
	if err != nil {
		return MarkReadResult{}, err
	}
	if len(ids) == 0 {
		return MarkReadResult{}, nil
	}
	updated := ids
	var remoteErr error
	if s.remote != nil {
		if updated, remoteErr = s.remote.SetSeenBulk(ctx, ids); len(updated) == 0 {
			return MarkReadResult{}, remoteErr
		}
	}
	value := true
	if err := s.repo.UpdateMessages(ctx, updated, ports.MessagePatch{IsRead: &value}); err != nil {
		return MarkReadResult{}, err
	}
	// One aggregate event, never one per message: the realtime hub drops a client
	// whose buffer fills, so a per-message burst would disconnect the very UI
	// waiting for the result.
	s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{"bulk": true, "count": len(updated)}})
	return MarkReadResult{Updated: len(updated), Capped: len(ids) >= markReadLimit}, remoteErr
}
