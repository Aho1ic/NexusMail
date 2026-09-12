package provider

import (
	"strings"
	"unicode"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

type Preset struct {
	Provider        domain.Provider
	IMAPHost        string
	IMAPPort        int
	IMAPTLSMode     string
	SMTPHost        string
	SMTPPort        int
	SMTPTLSMode     string
	AuthType        string
	ServerSavesSent bool
}

var presets = map[domain.Provider]Preset{
	domain.ProviderQQ: {
		Provider: domain.ProviderQQ, IMAPHost: "imap.qq.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.qq.com", SMTPPort: 465, SMTPTLSMode: "implicit", AuthType: "password", ServerSavesSent: false,
	},
	domain.Provider163: {
		Provider: domain.Provider163, IMAPHost: "imap.163.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.163.com", SMTPPort: 465, SMTPTLSMode: "implicit", AuthType: "password", ServerSavesSent: false,
	},
	// 126 is the same NetEase service under a second domain, so it inherits every
	// quirk 163 already drove: the RFC 2971 ID handshake on connect, the "too many
	// connections" rate-limit classification, and the absent \Archive special-use
	// attribute that makes the archive folder something this client has to create.
	domain.Provider126: {
		Provider: domain.Provider126, IMAPHost: "imap.126.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.126.com", SMTPPort: 465, SMTPTLSMode: "implicit", AuthType: "password", ServerSavesSent: false,
	},
	domain.ProviderGmail: {
		Provider: domain.ProviderGmail, IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLSMode: "implicit", AuthType: "oauth2", ServerSavesSent: true,
	},
	domain.ProviderOutlook: {
		Provider: domain.ProviderOutlook, IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587, SMTPTLSMode: "starttls", AuthType: "oauth2", ServerSavesSent: true,
	},
	// Apple publishes no OAuth endpoint for IMAP or SMTP, so iCloud authenticates
	// with an app-specific password like the Chinese providers rather than through
	// the OAuth flow its platform peers use. Its SMTP does not file sent mail
	// either, so ServerSavesSent stays false and the worker APPENDs to Sent.
	//
	// 587/STARTTLS is what Apple documents; the implicit-TLS port is not offered.
	domain.ProviderICloud: {
		Provider: domain.ProviderICloud, IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLSMode: "starttls", AuthType: "password", ServerSavesSent: false,
	},
}

func Get(name string) (Preset, error) {
	preset, ok := presets[domain.Provider(strings.ToLower(name))]
	if !ok {
		return Preset{}, ports.Invalidf("unsupported email provider")
	}
	return preset, nil
}

// roleRule maps a mailbox name to a role. ASCII keywords are matched as whole
// words, never as substrings: "Presentations" contains "sent" and used to be
// classified as the Sent folder, which let AppendSent write delivered mail into a
// user's own folder. CJK keywords have no word boundaries, so they stay
// substring matches — the terms are specific enough not to collide.
type roleRule struct {
	role     string
	syncMode string
	words    []string
	contains []string
}

// The order is significant: the first rule that matches wins, so "Deleted
// Messages" must be tested for trash before "messages" can mean anything else.
//
// "inbox" is last because it is the only keyword that is also the standard IMAP
// hierarchy prefix and a common container name: mailboxWords tokenises on
// non-alphanumerics, so "INBOX.Sent", "Inbox Backup" and anything containing
// 收件箱 all yield the "inbox" token. Matched first, such a folder became a decoy
// role='inbox' with sync_mode='realtime' that joined the unified inbox feed and,
// because GetMailboxByRole orders by id, could capture the 5s probe and the IDLE
// SELECT while real inbox mail waited for the 5-minute periodic pass. Every more
// specific role therefore gets first refusal. The real INBOX is unaffected: the
// exact-name fast path at normalized == "inbox" returns before this loop runs.
var roleRules = []roleRule{
	{role: "trash", syncMode: "lazy", words: []string{"trash", "deleted", "bin"}, contains: []string{"已删除", "废件箱", "回收站"}},
	{role: "junk", syncMode: "lazy", words: []string{"junk", "spam"}, contains: []string{"垃圾"}},
	{role: "sent", syncMode: "periodic", words: []string{"sent"}, contains: []string{"已发送", "发件箱"}},
	{role: "drafts", syncMode: "periodic", words: []string{"draft", "drafts"}, contains: []string{"草稿"}},
	// "所有邮件" is the localised every-message view, so it is lazy for the same
	// reason "All Mail" is, while a real archive folder stays on the periodic tick.
	{role: "archive", syncMode: "lazy", contains: []string{"所有邮件"}},
	{role: "archive", syncMode: "periodic", words: []string{"archive", "archives"}, contains: []string{"归档"}},
	{role: "inbox", syncMode: "realtime", words: []string{"inbox"}, contains: []string{"收件箱"}},
}

func ClassifyMailbox(name string, attributes []string) (role, syncMode string) {
	for _, attribute := range attributes {
		switch strings.ToLower(attribute) {
		case `\inbox`:
			return "inbox", "realtime"
		case `\sent`:
			return "sent", "periodic"
		case `\drafts`:
			return "drafts", "periodic"
		case `\archive`:
			return "archive", "periodic"
		case `\all`:
			// \All is not an archive folder, it is every message in the account —
			// Gmail's "All Mail" holds a second copy of the inbox, the sent folder and
			// everything ever archived. Syncing it on the periodic tick doubled the
			// first import and then re-walked the whole account history every five
			// minutes on the one command connection, which is what pushed new-mail
			// latency into the minutes. It syncs on demand like trash and junk.
			return "archive", "lazy"
		case `\trash`:
			return "trash", "lazy"
		case `\junk`:
			return "junk", "lazy"
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "inbox" {
		return "inbox", "realtime"
	}
	// "All Mail" is Gmail's every-message view and is two words, so it is checked
	// before the single-word rules rather than being expressed as one of them. It is
	// lazy for the same reason \All is.
	if hasPhrase(normalized, "all mail") {
		return "archive", "lazy"
	}
	words := mailboxWords(normalized)
	for _, rule := range roleRules {
		for _, word := range rule.words {
			if _, ok := words[word]; ok {
				return rule.role, rule.syncMode
			}
		}
		for _, fragment := range rule.contains {
			if strings.Contains(normalized, fragment) {
				return rule.role, rule.syncMode
			}
		}
	}
	return "custom", "lazy"
}

// mailboxWords splits a mailbox name on everything that is not a letter or
// digit, so "Sent Items", "sent-items", "INBOX.Sent" and "Sent_Mail" all yield
// the token "sent" while "Presentations" yields only itself.
func mailboxWords(normalized string) map[string]struct{} {
	fields := strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	words := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		words[field] = struct{}{}
	}
	return words
}

// hasPhrase matches a multi-word phrase on word boundaries.
func hasPhrase(normalized, phrase string) bool {
	index := strings.Index(normalized, phrase)
	if index < 0 {
		return false
	}
	if index > 0 && isWordByte(normalized[index-1]) {
		return false
	}
	tail := index + len(phrase)
	return tail >= len(normalized) || !isWordByte(normalized[tail])
}

func isWordByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
