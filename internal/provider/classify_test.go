package provider

import "testing"

// TestClassifyMailboxByName covers the name fallback, which is what most Chinese
// providers actually exercise: they rarely publish special-use attributes, so the
// role a mailbox gets — and with it its sync tier — comes down to this matching.
func TestClassifyMailboxByName(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		syncMode string
	}{
		{"INBOX", "inbox", "realtime"},
		{"收件箱", "inbox", "realtime"},
		{"Sent", "sent", "periodic"},
		{"Sent Messages", "sent", "periodic"},
		{"已发送邮件", "sent", "periodic"},
		{"Drafts", "drafts", "periodic"},
		{"草稿箱", "drafts", "periodic"},
		{"Trash", "trash", "lazy"},
		{"Deleted Items", "trash", "lazy"},
		{"已删除邮件", "trash", "lazy"},
		{"垃圾邮件", "junk", "lazy"},
		{"Junk E-mail", "junk", "lazy"},
		{"Archive", "archive", "periodic"},
		{"All Mail", "archive", "periodic"},
		{"Notes", "custom", "lazy"},
		{"Newsletters", "custom", "lazy"},
		// Substring matching used to answer these: "Presentations" contains "sent",
		// so a user folder was classified as Sent and demoted out of realtime sync
		// while its mail was filed as outgoing-ish. Token matching keeps them custom.
		{"Presentations", "custom", "lazy"},
		{"Assented", "custom", "lazy"},
		{"Junkyard Photos", "custom", "lazy"},
		{"Draftsman", "custom", "lazy"},
		{"Trashy Novels", "custom", "lazy"},
		{"Inboxing Tips", "custom", "lazy"},
	}
	for _, item := range cases {
		role, syncMode := ClassifyMailbox(item.name, nil)
		if role != item.role || syncMode != item.syncMode {
			t.Errorf("ClassifyMailbox(%q) = %q/%q, want %q/%q", item.name, role, syncMode, item.role, item.syncMode)
		}
	}
}

// TestClassifyMailboxAttributesWin asserts the declared attribute is not
// second-guessed by the name: a provider that labels a folder \Trash owns that
// answer even if it calls the folder something else.
func TestClassifyMailboxAttributesWin(t *testing.T) {
	role, syncMode := ClassifyMailbox("Inbox Backup", []string{"\\Trash"})
	if role != "trash" || syncMode != "lazy" {
		t.Fatalf("attribute ignored: got %q/%q", role, syncMode)
	}
	if role, syncMode = ClassifyMailbox("随便取的名字", []string{"\\Sent"}); role != "sent" || syncMode != "periodic" {
		t.Fatalf("sent attribute ignored: got %q/%q", role, syncMode)
	}
}
