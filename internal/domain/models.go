package domain

type Provider string

const (
	ProviderQQ      Provider = "qq"
	Provider163     Provider = "163"
	ProviderGmail   Provider = "gmail"
	ProviderOutlook Provider = "outlook"
)

type Account struct {
	ID               int64   `json:"id"`
	Email            string  `json:"email"`
	DisplayName      string  `json:"display_name"`
	Provider         string  `json:"provider"`
	AuthType         string  `json:"auth_type"`
	Username         string  `json:"username"`
	IMAPHost         string  `json:"-"`
	IMAPPort         int     `json:"-"`
	IMAPTLSMode      string  `json:"-"`
	SMTPHost         string  `json:"-"`
	SMTPPort         int     `json:"-"`
	SMTPTLSMode      string  `json:"-"`
	SecretCiphertext []byte  `json:"-"`
	Status           string  `json:"status"`
	LastError        *string `json:"last_error,omitempty"`
	LastConnectedAt  *int64  `json:"last_connected_at,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`
}

func (Account) TableName() string { return "accounts" }

type Mailbox struct {
	ID          int64   `json:"id"`
	AccountID   int64   `json:"account_id"`
	RemoteName  string  `json:"remote_name"`
	DisplayName string  `json:"display_name"`
	Delimiter   *string `json:"delimiter,omitempty"`
	Role        string  `json:"role"`
	SyncMode    string  `json:"sync_mode"`
	UIDValidity uint32  `json:"uid_validity"`
	UIDNext     *uint32 `json:"uid_next,omitempty"`
	// The column is highest_modseq, but GORM's naming strategy derives
	// highest_mod_seq from the field name, so without this tag the value was written
	// through raw column names and then never read back: every load returned nil.
	HighestModSeq *uint64 `json:"highest_modseq,omitempty" gorm:"column:highest_modseq"`
	LastUID       uint32  `json:"last_uid"`
	LastSyncAt    *int64  `json:"last_sync_at,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
}

func (Mailbox) TableName() string { return "mailboxes" }

type Message struct {
	ID                int64   `json:"id"`
	AccountID         int64   `json:"account_id"`
	Direction         string  `json:"direction"`
	DedupeKey         []byte  `json:"-"`
	RFCMessageID      *string `json:"rfc_message_id,omitempty"`
	ProviderMessageID *string `json:"provider_message_id,omitempty"`
	InReplyTo         *string `json:"in_reply_to,omitempty"`
	ReferencesJSON    string  `json:"references_json"`
	Subject           string  `json:"subject"`
	Sender            string  `json:"sender"`
	Recipients        string  `json:"recipients"`
	FromJSON          string  `json:"from"`
	ToJSON            string  `json:"to"`
	CCJSON            string  `json:"cc"`
	BCCJSON           string  `json:"bcc"`
	ReplyToJSON       string  `json:"reply_to"`
	Snippet           string  `json:"snippet"`
	BodyText          string  `json:"body_text,omitempty"`
	BodyHTML          string  `json:"body_html,omitempty"`
	BodyState         string  `json:"body_state"`
	RawBlobID         *int64  `json:"-"`
	SizeBytes         int64   `json:"size_bytes"`
	SentAt            *int64  `json:"sent_at,omitempty"`
	ReceivedAt        int64   `json:"received_at"`
	IsRead            bool    `json:"is_read"`
	IsStarred         bool    `json:"is_starred"`
	HasAttachments    bool    `json:"has_attachments"`
	CreatedAt         int64   `json:"created_at"`
	UpdatedAt         int64   `json:"updated_at"`
}

func (Message) TableName() string { return "messages" }

type Attachment struct {
	ID          int64   `json:"id"`
	MessageID   int64   `json:"message_id"`
	PartID      string  `json:"part_id"`
	Filename    string  `json:"filename"`
	ContentType string  `json:"content_type"`
	Disposition string  `json:"disposition"`
	ContentID   *string `json:"content_id,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	FetchState  string  `json:"fetch_state"`
	BlobID      *int64  `json:"-"`
	LastError   *string `json:"last_error,omitempty"`
	CreatedAt   int64   `json:"created_at"`
	UpdatedAt   int64   `json:"updated_at"`
}

func (Attachment) TableName() string { return "attachments" }

type Draft struct {
	ID                int64   `json:"id"`
	AccountID         int64   `json:"account_id"`
	SourceMessageID   *int64  `json:"source_message_id,omitempty"`
	ConflictOfID      *int64  `json:"conflict_of_id,omitempty"`
	SentMessageID     *int64  `json:"sent_message_id,omitempty"`
	RFCMessageID      string  `json:"rfc_message_id"`
	Revision          int64   `json:"revision"`
	ToJSON            string  `json:"to"`
	CCJSON            string  `json:"cc"`
	BCCJSON           string  `json:"bcc"`
	Subject           string  `json:"subject"`
	BodyText          string  `json:"body_text"`
	Status            string  `json:"status"`
	RemoteSyncState   string  `json:"remote_sync_state"`
	RemoteMailboxID   *int64  `json:"remote_mailbox_id,omitempty"`
	RemoteUIDValidity *uint32 `json:"remote_uid_validity,omitempty"`
	RemoteUID         *uint32 `json:"remote_uid,omitempty"`
	RemoteUpdatedAt   *int64  `json:"remote_updated_at,omitempty"`
	AttemptCount      int     `json:"attempt_count"`
	NextAttemptAt     *int64  `json:"next_attempt_at,omitempty"`
	LastSMTPCode      *int    `json:"last_smtp_code,omitempty"`
	LastError         *string `json:"last_error,omitempty"`
	SentAt            *int64  `json:"sent_at,omitempty"`
	CreatedAt         int64   `json:"created_at"`
	UpdatedAt         int64   `json:"updated_at"`
}

func (Draft) TableName() string { return "drafts" }

type DraftAttachment struct {
	ID          int64  `json:"id"`
	DraftID     int64  `json:"draft_id"`
	BlobID      int64  `json:"-"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Position    int    `json:"position"`
	CreatedAt   int64  `json:"created_at"`
}

func (DraftAttachment) TableName() string { return "draft_attachments" }

type BlobObject struct {
	ID             int64  `json:"id"`
	StorageKey     string `json:"storage_key"`
	SHA256         []byte `json:"-"`
	SizeBytes      int64  `json:"size_bytes"`
	Durability     string `json:"durability"`
	LastAccessedAt int64  `json:"last_accessed_at"`
	CreatedAt      int64  `json:"created_at"`
}

func (BlobObject) TableName() string { return "blob_objects" }
