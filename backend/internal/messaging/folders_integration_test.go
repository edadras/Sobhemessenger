package messaging_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Chat folders (§12).
//
// The claims worth pinning down are that a folder is a filter rather than a
// container, that an explicit exclusion beats the rules, and that the badge
// count and the list it describes never disagree.

func folderChatIDs(t *testing.T, repo *messaging.Repository, userID uuid.UUID, folderID *uuid.UUID) []uuid.UUID {
	t.Helper()
	chats, err := repo.ListChats(context.Background(), messaging.ChatListQuery{
		UserID: userID, Limit: 100, FolderID: folderID,
	})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(chats))
	for _, chat := range chats {
		ids = append(ids, chat.ID)
	}
	return ids
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func folderByID(t *testing.T, service *messaging.Service, userID, folderID uuid.UUID) messaging.Folder {
	t.Helper()
	folders, err := service.Folders(context.Background(), userID)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	for _, folder := range folders {
		if folder.ID == folderID {
			return folder
		}
	}
	t.Fatalf("folder %s is not in the list", folderID)
	return messaging.Folder{}
}

// botUser registers an account flagged as a bot, which one of the folder rules
// selects on.
func botUser(t *testing.T, db *database.DB, label string) uuid.UUID {
	t.Helper()
	id := createUser(t, db, label)
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE users SET is_bot = TRUE WHERE id = $1`, id); err != nil {
		t.Fatalf("mark bot: %v", err)
	}
	return id
}

func TestAFolderSelectsWholeCategories(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	friend := createUser(t, db, "friend")
	group := groupChat(t, db, owner, friend)
	private := privateChat(t, db, owner, friend)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Groups " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	ids := folderChatIDs(t, repo, owner, &folderID)
	if !contains(ids, group) {
		t.Error("a groups folder does not contain the group")
	}
	if contains(ids, private) {
		t.Error("a groups folder contains a one-to-one chat")
	}

	// The main list is unaffected: a folder is a view, not a move.
	all := folderChatIDs(t, repo, owner, nil)
	if !contains(all, group) || !contains(all, private) {
		t.Error("filing a chat in a folder took it out of the main list")
	}
}

func TestAnExplicitExclusionBeatsTheRules(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	first := groupChat(t, db, owner)
	second := groupChat(t, db, owner)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Groups " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	// "All my groups except that one" has no other expression.
	if err := service.SetFolderChat(ctx, owner, folderID, second, "exclude"); err != nil {
		t.Fatalf("SetFolderChat: %v", err)
	}

	ids := folderChatIDs(t, repo, owner, &folderID)
	if !contains(ids, first) {
		t.Error("the folder lost a group nothing excluded")
	}
	if contains(ids, second) {
		t.Error("an explicitly excluded chat is still in the folder")
	}
}

func TestAnExplicitInclusionBeatsTheRules(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	friend := createUser(t, db, "friend")
	private := privateChat(t, db, owner, friend)

	// A folder whose rules select nothing at all.
	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Hand-picked " + uuid.NewString()[:6],
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if got := folderChatIDs(t, repo, owner, &folderID); len(got) != 0 {
		t.Fatalf("a folder with no rules matched %d chats", len(got))
	}

	if err := service.SetFolderChat(ctx, owner, folderID, private, "include"); err != nil {
		t.Fatalf("SetFolderChat: %v", err)
	}
	if got := folderChatIDs(t, repo, owner, &folderID); !contains(got, private) {
		t.Error("a hand-picked chat is not in its folder")
	}
}

func TestContactsAndNonContactsAreDifferentRules(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	known := createUser(t, db, "known")
	unknown := createUser(t, db, "unknown")
	knownChat := privateChat(t, db, owner, known)
	unknownChat := privateChat(t, db, owner, unknown)

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO contacts (owner_id, contact_id) VALUES ($1, $2)`, owner, known); err != nil {
		t.Fatalf("add contact: %v", err)
	}

	contactsFolder, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "People " + uuid.NewString()[:6], IncludeContacts: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	strangersFolder, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Strangers " + uuid.NewString()[:6], IncludeNonContacts: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	inContacts := folderChatIDs(t, repo, owner, &contactsFolder)
	if !contains(inContacts, knownChat) || contains(inContacts, unknownChat) {
		t.Errorf("the contacts folder holds %v; it should hold only the chat with a contact", inContacts)
	}

	inStrangers := folderChatIDs(t, repo, owner, &strangersFolder)
	if !contains(inStrangers, unknownChat) || contains(inStrangers, knownChat) {
		t.Errorf("the non-contacts folder holds %v; it should hold only the chat with a stranger", inStrangers)
	}
}

func TestTheBotsRuleSelectsOnlyBots(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	bot := botUser(t, db, "helper")
	person := createUser(t, db, "person")
	botChat := privateChat(t, db, owner, bot)
	personChat := privateChat(t, db, owner, person)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Bots " + uuid.NewString()[:6], IncludeBots: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	ids := folderChatIDs(t, repo, owner, &folderID)
	if !contains(ids, botChat) {
		t.Error("the bots folder does not hold the chat with a bot")
	}
	if contains(ids, personChat) {
		t.Error("the bots folder holds a chat with a person")
	}
}

func TestExcludeReadHidesChatsWithNothingUnread(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	friend := createUser(t, db, "friend")
	quiet := groupChat(t, db, owner, friend)
	busy := groupChat(t, db, owner, friend)
	send(t, repo, busy, friend, "something new")

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Unread " + uuid.NewString()[:6], IncludeGroups: true, ExcludeRead: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	ids := folderChatIDs(t, repo, owner, &folderID)
	if !contains(ids, busy) {
		t.Error("an unread-only folder is missing a chat with unread messages")
	}
	if contains(ids, quiet) {
		t.Error("an unread-only folder holds a chat with nothing unread")
	}
}

func TestTheBadgeCountAgreesWithTheList(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	friend := createUser(t, db, "friend")
	inFolder := groupChat(t, db, owner, friend)
	outOfFolder := privateChat(t, db, owner, friend)

	send(t, repo, inFolder, friend, "one")
	send(t, repo, inFolder, friend, "two")
	send(t, repo, outOfFolder, friend, "not counted")

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Groups " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	folder := folderByID(t, service, owner, folderID)
	if folder.UnreadCount != 2 {
		t.Errorf("badge = %d, want 2 — the count must come from the same chats the list shows",
			folder.UnreadCount)
	}
	if folder.ChatCount != len(folderChatIDs(t, repo, owner, &folderID)) {
		t.Errorf("the folder claims %d chats but lists %d",
			folder.ChatCount, len(folderChatIDs(t, repo, owner, &folderID)))
	}
}

func TestAFolderIsPrivateToItsOwner(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	other := createUser(t, db, "other")
	chatID := groupChat(t, db, owner, other)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Mine " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if folders, err := service.Folders(ctx, other); err != nil {
		t.Fatalf("Folders: %v", err)
	} else if len(folders) != 0 {
		t.Errorf("another person sees %d of the owner's folders", len(folders))
	}
	if err := service.UpdateFolder(ctx, other, folderID, messaging.FolderInput{Title: "Theirs"}); err == nil {
		t.Error("someone else edited the owner's folder")
	}
	if err := service.SetFolderChat(ctx, other, folderID, chatID, "include"); err == nil {
		t.Error("someone else filed a chat into the owner's folder")
	}
	if err := service.DeleteFolder(ctx, other, folderID); err == nil {
		t.Error("someone else deleted the owner's folder")
	}
}

func TestAFolderCannotNameAChatTheCallerIsNotIn(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	stranger := createUser(t, db, "stranger")
	// A chat the owner has nothing to do with. Naming it in a folder would be
	// a way to ask whether it exists.
	elsewhere := groupChat(t, db, stranger)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Mine " + uuid.NewString()[:6],
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := service.SetFolderChat(ctx, owner, folderID, elsewhere, "include"); err == nil {
		t.Error("a folder named a chat the owner is not a member of")
	}
}

func TestDeletingAFolderLeavesTheChatsAlone(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Temporary " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := service.DeleteFolder(ctx, owner, folderID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}

	if !contains(folderChatIDs(t, repo, owner, nil), chatID) {
		t.Error("deleting a folder took its chats with it")
	}
}

func TestTwoFoldersCannotShareAName(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	name := "Work " + uuid.NewString()[:6]

	if _, err := service.CreateFolder(ctx, owner, messaging.FolderInput{Title: name}); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := service.CreateFolder(ctx, owner, messaging.FolderInput{Title: name}); err == nil {
		t.Error("two folders with the same name are indistinguishable in the tab strip")
	}
}

func TestAChatIsNeverBothIncludedAndExcluded(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)

	folderID, err := service.CreateFolder(ctx, owner, messaging.FolderInput{
		Title: "Groups " + uuid.NewString()[:6], IncludeGroups: true,
	})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if err := service.SetFolderChat(ctx, owner, folderID, chatID, "exclude"); err != nil {
		t.Fatalf("SetFolderChat: %v", err)
	}
	if err := service.SetFolderChat(ctx, owner, folderID, chatID, "include"); err != nil {
		t.Fatalf("SetFolderChat: %v", err)
	}

	folder := folderByID(t, service, owner, folderID)
	if len(folder.ExcludedChatIDs) != 0 {
		t.Errorf("the chat is still listed as excluded: %v", folder.ExcludedChatIDs)
	}
	if !contains(folderChatIDs(t, repo, owner, &folderID), chatID) {
		t.Error("switching a chat from excluded to included left it out")
	}
}

func TestReorderingSetsTheTabOrder(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	first, err := service.CreateFolder(ctx, owner, messaging.FolderInput{Title: "A " + uuid.NewString()[:6]})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	second, err := service.CreateFolder(ctx, owner, messaging.FolderInput{Title: "B " + uuid.NewString()[:6]})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if err := service.ReorderFolders(ctx, owner, []uuid.UUID{second, first}); err != nil {
		t.Fatalf("ReorderFolders: %v", err)
	}

	folders, err := service.Folders(ctx, owner)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[0].ID != second || folders[1].ID != first {
		t.Errorf("the tab order was not applied: %v", folders)
	}
}
