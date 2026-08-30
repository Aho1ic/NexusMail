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
	// UnreadTotal counts every unread message the view contains, not just the
	// ones on this page. The client cannot derive it from Items: a page holds at
	// most 100 rows while the view may hold years of unread mail.
	UnreadTotal int `json:"unread_total"`
}

// MessageInput is the batch ingest shape the supervisor passes to a syncMailbox
// pass. The message is fully constructed by the caller; the repository only
// decides insert vs update based on (account_id, dedupe_key), persists the
// mailbox mapping, and returns the row id it ended up using.
type MessageInput struct {
	Message      *domain.Message
	MailboxID    int64
	UID          uint32
	Flags        []string
	InternalDate time.Time
}

// RemoteFlagState is one message as the provider currently sees it, used to
// reconcile local rows against the server rather than only appending new mail.
type RemoteFlagState struct {
	UID       uint32
	IsRead    bool
	IsStarred bool
	Flags     []string
}

type MessageLocation struct {
	MessageID int64
	Account   domain.Account
	Mailbox   domain.Mailbox
	UID       uint32
}

type MessagePatch struct {
	IsRead    *bool
	IsStarred *bool
}

// The interfaces below slice persistence by subject area. Repository is their sum
// and is what main.go wires, but no consumer should take Repository just because
// it is available: each package declares the narrowest interface it actually uses
// (see the *Store interfaces in the service packages), which is what makes a
// hand-written fake in a test a few methods rather than sixty.

// AccountRepo persists accounts and their connection status.
type AccountRepo interface {
	CreateAccount(context.Context, *domain.Account) error
	GetAccount(context.Context, int64) (domain.Account, error)
	ListAccounts(context.Context) ([]domain.Account, error)
	UpdateAccountStatus(context.Context, int64, string, *string) error
}

// MailboxRepo persists the mailbox catalog and each mailbox's sync cursor.
type MailboxRepo interface {
	UpsertMailbox(context.Context, *domain.Mailbox) error
	ListMailboxes(context.Context, int64) ([]domain.Mailbox, error)
	GetMailbox(context.Context, int64) (domain.Mailbox, error)
	GetMailboxByRole(context.Context, int64, string) (domain.Mailbox, error)
	UpdateMailboxCursor(context.Context, int64, uint32, uint32, *uint32, *uint64) error
	ResetMailbox(context.Context, int64, uint32) error
	// ListMailboxUIDs returns the UIDs stored locally for one mailbox, ascending.
	// Reconciliation is driven from this list so its cost scales with what the app
	// actually holds rather than with the size of the remote mailbox.
	ListMailboxUIDs(context.Context, int64) ([]uint32, error)
	DeleteMailboxUIDs(context.Context, int64, []uint32) (int, error)
}

// MessageRepo persists messages, their bodies and their attachments.
type MessageRepo interface {
	// BatchCreateOrUpdateMessages ingests a batch of fetched messages under a
	// single writeMu and a single transaction. The result slices are parallel
	// to items: resultIDs[i] is the row id used for items[i], and
	// created[i] reports whether the dedupe_key was new.
	BatchCreateOrUpdateMessages(context.Context, []MessageInput) ([]int64, []bool, error)
	// BatchUpsertAttachments writes a batch of attachments in a single
	// transaction. Conflicts on (message_id, part_id) update the existing row.
	BatchUpsertAttachments(context.Context, []domain.Attachment) error
	ReconcileMailboxFlags(context.Context, int64, []RemoteFlagState) (int, error)
	MessageLocation(context.Context, int64) (MessageLocation, error)
	MessageLocations(context.Context, []int64) ([]MessageLocation, error)
	MoveMessageLocation(context.Context, int64, int64, int64, *uint32) error
	SetMessageBodyState(context.Context, int64, string) error
	BatchSetMessageBodyState(context.Context, []int64, string) error
	UpdateMessageBody(context.Context, int64, string, string, string, *int64) error
	GetAttachment(context.Context, int64, int64) (domain.Attachment, error)
	UpdateAttachmentBlob(context.Context, int64, int64) error
	ListBodyCandidateIDs(context.Context, int64, int64, int) ([]int64, error)
	ListMessages(context.Context, MessageFilter) (MessagePage, error)
	UnreadMessageIDs(context.Context, MessageFilter, int) ([]int64, error)
	GetMessage(context.Context, int64) (domain.Message, []domain.Attachment, error)
	UpdateMessage(context.Context, int64, MessagePatch) (domain.Message, error)
	UpdateMessages(context.Context, []int64, MessagePatch) error
	CreateSentMessage(context.Context, *domain.Message, int64) error
}

// DraftRepo persists drafts, their attachments and their remote mirror state.
type DraftRepo interface {
	CreateDraft(context.Context, *domain.Draft) error
	ListDrafts(context.Context, string) ([]domain.Draft, error)
	GetDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	UpdateDraft(context.Context, *domain.Draft, int64) error
	ReconcileRemoteDraft(context.Context, *domain.Draft) (domain.Draft, bool, error)
	UpdateDraftRemote(context.Context, int64, int64, uint32, uint32, int64, string, *string) error
	DeleteDraft(context.Context, int64) error
	AddDraftAttachment(context.Context, *domain.DraftAttachment) error
	DeleteDraftAttachment(context.Context, int64, int64) error
}

// OutboxRepo drives the send state machine.
type OutboxRepo interface {
	ClaimSendableDraft(context.Context, int64) (domain.Draft, []domain.DraftAttachment, error)
	ListDueDraftIDs(context.Context, int64) ([]int64, error)
	RecoverSendingDrafts(context.Context) error
	SetDraftDelivery(context.Context, int64, string, int, *int64, *int, *string, *int64) error
}

// BlobRepo is the index over content-addressed blobs, including what the LRU
// eviction pass needs to make its decision.
type BlobRepo interface {
	CreateBlob(context.Context, *domain.BlobObject) error
	GetBlob(context.Context, int64) (domain.BlobObject, error)
	DeleteBlob(context.Context, int64) error
	CachedBlobs(context.Context) ([]domain.BlobObject, error)
	CachedBlobBytes(context.Context) (int64, error)
}

// SessionRepo persists browser sessions by token hash.
type SessionRepo interface {
	CreateSession(context.Context, []byte, []byte, int64, int64) error
	ValidateSession(context.Context, []byte, int64) ([]byte, bool, error)
	DeleteSession(context.Context, []byte) error
	DeleteExpiredSessions(context.Context, int64) error
}

// Repository is every subject area plus the connection lifecycle. Only the
// process assembly in main.go and the components that genuinely span most of the
// schema (the IMAP supervisor) should depend on this breadth.
type Repository interface {
	AccountRepo
	MailboxRepo
	MessageRepo
	DraftRepo
	OutboxRepo
	BlobRepo
	SessionRepo
	Ping(context.Context) error
	Close() error
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
