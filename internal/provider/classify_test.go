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
		// "All Mail" and its localised form are every message in the account, not an
		// archive folder: on Gmail they hold a second copy of the inbox and of
		// everything ever sent. Syncing that on the periodic tier doubled the first
		// import and re-walked the whole account every five minutes on the connection
		// new mail needs, so they sync on demand like trash and junk.
		{"All Mail", "archive", "lazy"},
		{"[Gmail]/All Mail", "archive", "lazy"},
		{"所有邮件", "archive", "lazy"},
		{"归档", "archive", "periodic"},
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
	// \All and \Archive both mean "archive" as a role but not as a workload: \All is
	// the whole account, so it must not join the periodic tier that shares the
	// command connection with new-mail sync.
	if role, syncMode = ClassifyMailbox("[Gmail]/All Mail", []string{"\\All"}); role != "archive" || syncMode != "lazy" {
		t.Fatalf(`\All is not lazy: got %q/%q`, role, syncMode)
	}
	if role, syncMode = ClassifyMailbox("Archive", []string{"\\Archive"}); role != "archive" || syncMode != "periodic" {
		t.Fatalf(`\Archive changed tier: got %q/%q`, role, syncMode)
	}
}
