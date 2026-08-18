package ports

import (
	"context"
	"io"
	"time"

	"nexusmail/internal/domain"
)

type MessageFilter struct {
	AccountID *int64
	MailboxID *int64
	Folder    string
	IsRead    *bool
	Query     string
	Cursor    string
	Limit     int
}

type MessagePage struct {
	Items      []domain.Message `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type MessageLocation struct {
	Account domain.Account
	Mailbox domain.Mailbox
	UID     uint32
}

type MessagePatch struct {
	IsRead    *bool
	IsStarred *bool
}

type Repository interface {
	Ping(context.Context) error
	Close() error
	CreateAccount(context.Context, *domain.Account) error
	GetAccount(context.Context, int64) (domain.Account, error)
	ListAccounts(context.Context) ([]domain.Account, error)
	UpdateAccountStatus(context.Context, int64, string, *string) error
	UpsertMailbox(context.Context, *domain.Mailbox) error
	ListMailboxes(context.Context, int64) ([]domain.Mailbox, error)
	GetMailbox(context.Context, int64) (domain.Mailbox, error)
	GetMailboxByRole(context.Context, int64, string) (domain.Mailbox, error)
	UpdateMailboxCursor(context.Context, int64, uint32, uint32, *uint32, *uint64) error
	ResetMailbox(context.Context, int64, uint32) error
	CreateOrUpdateMessage(context.Context, *domain.Message, int64, uint32, []string, time.Time) (bool, error)
	MessageLocation(context.Context, int64) (MessageLocation, error)
	MoveMessageLocation(context.Context, int64, int64, int64, *uint32) error
	SetMessageBodyState(context.Context, int64, string) error
	UpdateMessageBody(context.Context, int64, string, string, string, *int64) error
	UpsertAttachment(context.Context, *domain.Attachment) error
	GetAttachment(context.Context, int64, int64) (domain.Attachment, error)
	UpdateAttachmentBlob(context.Context, int64, int64) error
	ListBodyCandidateIDs(context.Context, int64, int64, int) ([]int64, error)
	ListMessages(context.Context, MessageFilter) (MessagePage, error)
	GetMessage(context.Context, int64) (domain.Message, []domain.Attachment, error)
	UpdateMessage(context.Context, int64, MessagePatch) (domain.Message, error)
	CreateDraft(context.Context, *domain.Draft) error
	ListDrafts(context.Context, string) ([]domain.Draft, error)
	GetDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	UpdateDraft(context.Context, *domain.Draft, int64) error
	ReconcileRemoteDraft(context.Context, *domain.Draft) (domain.Draft, bool, error)
	UpdateDraftRemote(context.Context, int64, int64, uint32, uint32, int64, string, *string) error
	DeleteDraft(context.Context, int64) error
	CreateBlob(context.Context, *domain.BlobObject) error
	GetBlob(context.Context, int64) (domain.BlobObject, error)
	DeleteBlob(context.Context, int64) error
	CachedBlobs(context.Context) ([]domain.BlobObject, error)
	AddDraftAttachment(context.Context, *domain.DraftAttachment) error
	DeleteDraftAttachment(context.Context, int64, int64) error
	CreateSession(context.Context, []byte, []byte, int64, int64) error
	ValidateSession(context.Context, []byte, int64) ([]byte, bool, error)
	DeleteSession(context.Context, []byte) error
	DeleteExpiredSessions(context.Context, int64) error
	ClaimSendableDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	ListDueDraftIDs(context.Context, int64) ([]int64, error)
	RecoverSendingDrafts(context.Context) error
	SetDraftDelivery(context.Context, int64, string, int, *int64, *int, *string, *int64) error
	CreateSentMessage(context.Context, *domain.Message, int64) error
}

type BlobStore interface {
	Put(context.Context, io.Reader, string) (domain.BlobObject, error)
	Open(context.Context, domain.BlobObject) (io.ReadCloser, error)
	Remove(context.Context, domain.BlobObject) error
	Evict(context.Context) error
}

type Publisher interface {
	Publish(event Event)
}

type Event struct {
	Type       string `json:"type"`
	Sequence   uint64 `json:"sequence"`
	OccurredAt int64  `json:"occurred_at"`
	Data       any    `json:"data"`
}
