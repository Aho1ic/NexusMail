package provider

import (
	"errors"
	"strings"

	"nexusmail/internal/domain"
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
	domain.ProviderGmail: {
		Provider: domain.ProviderGmail, IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPTLSMode: "implicit", AuthType: "oauth2", ServerSavesSent: true,
	},
	domain.ProviderOutlook: {
		Provider: domain.ProviderOutlook, IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587, SMTPTLSMode: "starttls", AuthType: "oauth2", ServerSavesSent: true,
	},
}

func Get(name string) (Preset, error) {
	preset, ok := presets[domain.Provider(strings.ToLower(name))]
	if !ok {
		return Preset{}, errors.New("unsupported email provider")
	}
	return preset, nil
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
		case `\archive`, `\all`:
			return "archive", "periodic"
		case `\trash`:
			return "trash", "lazy"
		case `\junk`:
			return "junk", "lazy"
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch {
	case normalized == "inbox" || normalized == "收件箱":
		return "inbox", "realtime"
	case strings.Contains(normalized, "sent") || strings.Contains(normalized, "已发送"):
		return "sent", "periodic"
	case strings.Contains(normalized, "draft") || strings.Contains(normalized, "草稿"):
		return "drafts", "periodic"
	case strings.Contains(normalized, "archive") || strings.Contains(normalized, "归档") || strings.Contains(normalized, "all mail"):
		return "archive", "periodic"
	case strings.Contains(normalized, "trash") || strings.Contains(normalized, "deleted") || strings.Contains(normalized, "已删除"):
		return "trash", "lazy"
	case strings.Contains(normalized, "junk") || strings.Contains(normalized, "spam") || strings.Contains(normalized, "垃圾"):
		return "junk", "lazy"
	default:
		return "custom", "lazy"
	}
}
