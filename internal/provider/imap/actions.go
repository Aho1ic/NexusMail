package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// User-initiated operations on existing mail: flags, archive, and bulk mark-read.

// onMessageConn runs fn against the command connection of whichever account owns
// messageID, holding the foreground lock for the duration.
//
// The lock is not a parameter. Every operation on the command connection has to
// choose between rt.lock() and rt.lockBackground(), and a user action waiting on a
// keypress is always the foreground one — picking the background lock here would
// queue the click behind the body-prefetch backlog, which is the minutes-long
// stall this codebase already paid for once. Baking the choice in is what stops a
// new action from getting it wrong.
//
// The two callers that do not use this need something it cannot express:
// fetchBody chooses its lock at runtime from its background flag, and
// FetchAttachment parses the part ID between resolving the runtime and taking the
// lock so a malformed ID fails without touching the connection.
func (s *Supervisor) onMessageConn(ctx context.Context, messageID int64, fn func(*runtime, *imapclient.Client, ports.MessageLocation) error) error {
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	return fn(rt, client, location)
}

func (s *Supervisor) SetFlags(ctx context.Context, messageID int64, isRead, isStarred *bool) error {
	return s.onMessageConn(ctx, messageID, func(_ *runtime, client *imapclient.Client, location ports.MessageLocation) error {
		if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
			return err
		}
		uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
		// Ordered on purpose: a map range would emit the two STOREs in a random order,
		// so a provider that only reports the last one, or a test asserting the wire
		// sequence, would see different behaviour run to run.
		for _, update := range []struct {
			flag  goimap.Flag
			value *bool
		}{
			{goimap.FlagSeen, isRead},
			{goimap.FlagFlagged, isStarred},
		} {
			flag, value := update.flag, update.value
			if value == nil {
				continue
			}
			op := goimap.StoreFlagsDel
			if *value {
				op = goimap.StoreFlagsAdd
			}
			if _, err := client.Store(uidSet, &goimap.StoreFlags{Op: op, Silent: true, Flags: []goimap.Flag{flag}}, nil).Collect(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Supervisor) Archive(ctx context.Context, messageID int64) error {
	return s.onMessageConn(ctx, messageID, func(rt *runtime, client *imapclient.Client, location ports.MessageLocation) error {
		return s.archiveOn(ctx, rt, client, messageID, location)
	})
}

func (s *Supervisor) archiveOn(ctx context.Context, rt *runtime, client *imapclient.Client, messageID int64, location ports.MessageLocation) error {
	destination, err := s.ensureArchiveMailbox(ctx, rt, client)
	if err != nil {
		return err
	}
	if destination.ID == location.Mailbox.ID {
		return nil
	}
	if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
	if client.Caps().Has(goimap.CapMove) {
		data, moveErr := client.Move(uidSet, destination.RemoteName).Wait()
		if moveErr != nil {
			return moveErr
		}
		return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, firstDestinationUID(data.DestUIDs))
	}
	// COPY + \Deleted + expunge is the fallback. The expunge is not optional: the
	// local row is dropped from the source mailbox either way, so a message left
	// behind on the server is one the user archived here and still sees in the
	// provider's own web client. QQ advertises neither MOVE nor UIDPLUS on some
	// connections, which is exactly the path that has to remove it.
	expungeSafe, err := noPendingDeletes(client)
	if err != nil {
		return err
	}
	copyData, err := client.Copy(uidSet, destination.RemoteName).Wait()
	if err != nil {
		return err
	}
	if _, err = client.Store(uidSet, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	switch {
	case client.Caps().Has(goimap.CapUIDPlus):
		if _, err = client.UIDExpunge(uidSet).Collect(); err != nil {
			return err
		}
	case expungeSafe:
		// Plain EXPUNGE removes every \Deleted message in the mailbox, so it is only
		// safe when this message is the only one carrying the flag. Another client's
		// pending deletes must not be finalised as a side effect of archiving.
		if _, err = client.Expunge().Collect(); err != nil {
			return err
		}
	default:
		slog.Warn("archive left message on server: mailbox has other \\Deleted messages and provider lacks UIDPLUS",
			"account_id", location.Account.ID, "mailbox", location.Mailbox.RemoteName, "uid", location.UID)
	}
	var destinationUID *uint32
	if copyData != nil {
		destinationUID = firstDestinationUID(copyData.DestUIDs)
	}
	return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, destinationUID)
}

// noPendingDeletes reports whether the selected mailbox currently holds no
// message flagged \Deleted, which is the precondition for a plain EXPUNGE being
// equivalent to expunging one UID.
//
// It is deliberately conservative: only a search that came back and was empty
// answers true. An error, or a response whose set cannot be read as UIDs, answers
// false, because losing another client's pending deletes is worse than leaving one
// archived message on the server.
func noPendingDeletes(client *imapclient.Client) (bool, error) {
	data, err := client.UIDSearch(&goimap.SearchCriteria{Flag: []goimap.Flag{goimap.FlagDeleted}}, nil).Wait()
	if err != nil {
		return false, err
	}
	if data.All == nil {
		return true, nil
	}
	set, ok := data.All.(goimap.UIDSet)
	if !ok {
		return false, nil
	}
	// A dynamic set ("*") is a server bug that AllUIDs would panic on; treat it as
	// unknown rather than empty.
	uids, ok := set.Nums()
	if !ok {
		return false, nil
	}
	return len(uids) == 0, nil
}

// archiveCandidateNames are the mailbox names tried when creating an archive
// folder, in order. Each must classify as the archive role via
// provider.ClassifyMailbox, otherwise the folder would be created and then not
// found. The plain root-level name comes first because that is where a provider
// which allows it puts a real sibling of INBOX.
var archiveCandidateNames = []string{"Archive", "Archives"}

// ensureArchiveMailbox returns the account's archive mailbox, creating one on the
// provider when none exists.
//
// QQ and 163 ship no archive folder and advertise no \Archive special-use
// attribute, so the role was simply absent and every archive attempt failed with
// "archive mailbox is unavailable". Creating the folder is what makes the action
// mean something on those providers, and it is done once: the second call finds
// the role and returns immediately.
//
// The caller must hold the command lock — this issues CREATE and LIST on the
// command connection.
func (s *Supervisor) ensureArchiveMailbox(ctx context.Context, rt *runtime, client *imapclient.Client) (domain.Mailbox, error) {
	if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
		return mailbox, nil
	} else if !errors.Is(err, ports.ErrNotFound) {
		return domain.Mailbox{}, err
	}
	// The catalog may predate a folder created in another client. Re-list before
	// concluding the account has no archive at all.
	items, err := s.refreshMailboxCatalog(ctx, rt, client)
	if err != nil {
		return domain.Mailbox{}, err
	}
	if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
		return mailbox, nil
	} else if !errors.Is(err, ports.ErrNotFound) {
		return domain.Mailbox{}, err
	}
	var createErrs []error
	for _, name := range archiveCandidateNames {
		for _, candidate := range archivePaths(name, items) {
			if err := client.Create(candidate, archiveCreateOptions(client)).Wait(); err != nil {
				createErrs = append(createErrs, fmt.Errorf("create %q: %w", candidate, err))
				continue
			}
			slog.Info("created archive mailbox", "account_id", rt.account.ID, "mailbox", candidate)
			// Trust LIST, not the name that was requested: a provider is free to
			// place the folder somewhere else, and the stored remote name has to be
			// the one SELECT and COPY will accept.
			if _, err := s.refreshMailboxCatalog(ctx, rt, client); err != nil {
				return domain.Mailbox{}, err
			}
			if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
				return mailbox, nil
			}
			return domain.Mailbox{}, fmt.Errorf("created archive mailbox %q but it did not classify as archive", candidate)
		}
	}
	if len(createErrs) > 0 {
		return domain.Mailbox{}, ports.Unavailablef("archive mailbox is unavailable: %w", errors.Join(createErrs...))
	}
	return domain.Mailbox{}, ports.Unavailablef("archive mailbox is unavailable")
}

// archiveCreateOptions asks for the \Archive special-use attribute when the
// provider supports CREATE-SPECIAL-USE, so a server that understands roles
// records this folder as the archive for every other client too. QQ and 163 do
// not advertise it and get a plain CREATE.
func archiveCreateOptions(client *imapclient.Client) *goimap.CreateOptions {
	if !client.Caps().Has(goimap.Cap("CREATE-SPECIAL-USE")) {
		return nil
	}
	return &goimap.CreateOptions{SpecialUse: []goimap.MailboxAttr{goimap.MailboxAttrArchive}}
}

// archivePaths returns where to try creating an archive folder called name: at
// the root first, then under each \Noselect container the provider exposes.
//
// QQ accepts both, but a provider that only allows user folders beneath a
// container ("其他文件夹" on QQ, "[Gmail]" on Gmail) rejects the root-level CREATE,
// and the nested path is the one that works there. \Noselect is the server's own
// statement that a folder holds only children, which is why the attribute is used
// rather than guessing from the stored role — a top-level folder the user created
// for their own mail must never become the archive's parent.
func archivePaths(name string, items []*goimap.ListData) []string {
	paths := []string{name}
	for _, item := range items {
		if item.Delim == 0 {
			continue
		}
		noselect := false
		for _, attr := range item.Attrs {
			if attr == goimap.MailboxAttrNoSelect {
				noselect = true
				break
			}
		}
		if !noselect {
			continue
		}
		paths = append(paths, item.Mailbox+string(item.Delim)+name)
	}
	return paths
}

// SetSeenBulk adds \Seen to many messages, grouped so each account's command
// connection is taken once. It returns the message IDs the provider accepted so
// the caller only writes through the rows that really changed remotely.
//
// Failures are per account: one offline mailbox must not discard the flags that
// other accounts already stored.
func (s *Supervisor) SetSeenBulk(ctx context.Context, messageIDs []int64) ([]int64, error) {
	locations, err := s.repo.MessageLocations(ctx, messageIDs)
	if err != nil {
		return nil, err
	}
	groups := make(map[int64]map[int64][]ports.MessageLocation)
	for _, location := range locations {
		byMailbox := groups[location.Account.ID]
		if byMailbox == nil {
			byMailbox = make(map[int64][]ports.MessageLocation)
			groups[location.Account.ID] = byMailbox
		}
		byMailbox[location.Mailbox.ID] = append(byMailbox[location.Mailbox.ID], location)
	}
	done := make([]int64, 0, len(locations))
	var failures []error
	for accountID, byMailbox := range groups {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		updated, err := s.setSeenAccount(ctx, accountID, byMailbox)
		done = append(done, updated...)
		if err != nil {
			failures = append(failures, fmt.Errorf("account %d: %w", accountID, err))
		}
	}
	return done, errors.Join(failures...)
}

// setSeenAccount holds one account's command lock for the duration of one
// mailbox at a time, not for the whole account. The 5s inbox probe and the
// IDLE-driven sync contend for the same lock, so a slow mailbox (large chunk,
// network latency) cannot block the new-mail path for the rest of the
// account. The lock is released and re-taken per mailbox; the IMAP connection
// itself is shared and stays put.
func (s *Supervisor) setSeenAccount(ctx context.Context, accountID int64, byMailbox map[int64][]ports.MessageLocation) ([]int64, error) {
	rt, err := s.runtime(accountID)
	if err != nil {
		return nil, err
	}
	done := make([]int64, 0)
	var failures []error
	for _, group := range byMailbox {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		rt.lock()
		client := rt.client.Load()
		if client == nil {
			rt.unlock()
			return nil, ports.Unavailablef("account is offline")
		}
		if _, err := client.Select(group[0].Mailbox.RemoteName, nil).Wait(); err != nil {
			failures = append(failures, fmt.Errorf("select %q: %w", group[0].Mailbox.RemoteName, err))
			rt.unlock()
			continue
		}
		for start := 0; start < len(group); start += bulkFlagChunk {
			end := min(start+bulkFlagChunk, len(group))
			chunk := group[start:end]
			uids := make([]goimap.UID, len(chunk))
			for index, location := range chunk {
				uids[index] = goimap.UID(location.UID)
			}
			store := &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagSeen}}
			if _, err := client.Store(goimap.UIDSetNum(uids...), store, nil).Collect(); err != nil {
				failures = append(failures, err)
				continue
			}
			for _, location := range chunk {
				done = append(done, location.MessageID)
			}
		}
		rt.unlock()
	}
	return done, errors.Join(failures...)
}

func firstDestinationUID(set goimap.NumSet) *uint32 {
	uids, ok := set.(goimap.UIDSet)
	if !ok {
		return nil
	}
	values, ok := uids.Nums()
	if !ok || len(values) == 0 {
		return nil
	}
	value := uint32(values[0])
	return &value
}
