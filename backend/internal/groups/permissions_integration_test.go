package groups_test

import (
	"context"
	"testing"

	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/stories"
)

// The permission vocabulary (§14).
//
// `group_permissions` has been in the schema since migration 0005 with a
// comment saying member permissions validate against it. Nothing did, so a
// misspelled key was stored, reported as applied, and silently ignored — the
// one outcome that makes a typo indistinguishable from a broken feature.

func TestTheVocabularyMatchesTheTableItComesFrom(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	rows, err := db.Pool.Query(ctx, `SELECT key, applies_to FROM group_permissions`)
	if err != nil {
		t.Fatalf("read group_permissions: %v", err)
	}
	defer rows.Close()

	stored := map[string]string{}
	for rows.Next() {
		var key, appliesTo string
		if err := rows.Scan(&key, &appliesTo); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stored[key] = appliesTo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// The Go copy exists so an administrative check costs no round trip. This
	// is what stops the two drifting: a key added to one and not the other
	// fails the build rather than becoming a permission that does nothing.
	for key, appliesTo := range stored {
		got, ok := messaging.PermissionKeys[key]
		if !ok {
			t.Errorf("the schema declares %q and the resolver has never heard of it", key)
			continue
		}
		if got != appliesTo {
			t.Errorf("%q applies to %q in the schema and %q in the resolver", key, appliesTo, got)
		}
	}
	for key := range messaging.PermissionKeys {
		if _, ok := stored[key]; !ok {
			t.Errorf("the resolver knows %q and the schema does not declare it", key)
		}
	}
}

func TestEveryDeclaredPermissionCanActuallyBeApplied(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	messagingRepo := messaging.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Vocabulary", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Granting every group-applicable key at once and reading the result back
	// catches a key that is stored but has no field behind it: the resolver
	// would return false for it however it was granted.
	granted := map[string]bool{}
	for key, appliesTo := range messaging.PermissionKeys {
		if appliesTo == "channel" {
			continue
		}
		granted[key] = true
	}
	if err := repo.SetRole(ctx, chatID, member, messaging.RoleMember, granted, ""); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	chatCtx, err := messagingRepo.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	effective := map[string]bool{
		"send_messages":   chatCtx.Permissions.SendMessages,
		"send_media":      chatCtx.Permissions.SendMedia,
		"send_files":      chatCtx.Permissions.SendFiles,
		"send_polls":      chatCtx.Permissions.SendPolls,
		"send_stickers":   chatCtx.Permissions.SendStickers,
		"embed_links":     chatCtx.Permissions.EmbedLinks,
		"add_members":     chatCtx.Permissions.AddMembers,
		"remove_members":  chatCtx.Permissions.RemoveMembers,
		"ban_members":     chatCtx.Permissions.BanMembers,
		"pin_messages":    chatCtx.Permissions.PinMessages,
		"edit_group":      chatCtx.Permissions.EditGroup,
		"delete_messages": chatCtx.Permissions.DeleteMessages,
		"manage_admins":   chatCtx.Permissions.ManageAdmins,
		"manage_calls":    chatCtx.Permissions.ManageCalls,
		"manage_invites":  chatCtx.Permissions.ManageInvites,
		"post_stories":    chatCtx.Permissions.PostStories,
	}
	for key := range granted {
		if !effective[key] {
			t.Errorf("%q was granted and did nothing", key)
		}
	}
}

func TestAMisspelledPermissionIsRefusedRatherThanIgnored(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Singular where the vocabulary is plural: the kind of mistake that used
	// to return 200 and change nothing.
	err := h.service.SetRole(ctx, chatID, owner, member, messaging.RoleMember,
		map[string]bool{"send_message": false}, "")
	if err == nil {
		t.Fatal("a misspelled permission key was accepted")
	}

	if err := h.service.SetRole(ctx, chatID, owner, member, messaging.RoleMember,
		map[string]bool{"send_messages": false}, ""); err != nil {
		t.Fatalf("the correctly spelled key was refused: %v", err)
	}
}

func TestAChannelOnlyPermissionIsRefusedInAGroup(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	groupID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)

	if err := h.service.UpdateSettings(ctx, groupID, owner, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000,
		DefaultPermissions: map[string]bool{"post_stories": true},
	}); err == nil {
		t.Error("a channel-only permission was accepted on a group")
	}

	// The same key on a channel is fine, which is what makes the refusal
	// above about applicability rather than about the key existing.
	if err := h.service.UpdateSettings(ctx, channelID, owner, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000,
		DefaultPermissions: map[string]bool{"post_stories": true},
	}); err != nil {
		t.Errorf("post_stories was refused on a channel: %v", err)
	}
}

func TestGrantingAnUnknownKeyThroughSettingsIsRefused(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)

	if err := h.service.UpdateSettings(ctx, chatID, owner, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000,
		DefaultPermissions: map[string]bool{"be_excellent": false},
	}); err == nil {
		t.Error("an invented permission key was accepted chat-wide")
	}
}

// ---------------------------------------------------- posting as a channel

func TestOnlyChannelStaffMayPublishAStoryAsTheChannel(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	outsider := createUser(t, h.db, "outsider")
	subscriber := createUser(t, h.db, "subscriber")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, true)
	if err := h.repo.AddMember(ctx, channelID, subscriber, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	service := stories.NewService(stories.NewRepository(h.db), h.messaging)

	// The channel id comes from the request body. Until it was checked, this
	// published a story that appeared to come from the channel.
	if _, err := service.Create(ctx, stories.CreateInput{
		AuthorID: outsider, ChannelChatID: &channelID,
		Type: "text", Caption: "not from the newsroom",
	}); err == nil {
		t.Error("an outsider published a story as a public channel")
	}

	// A subscriber is a member and still not staff.
	if _, err := service.Create(ctx, stories.CreateInput{
		AuthorID: subscriber, ChannelChatID: &channelID,
		Type: "text", Caption: "nor from a reader",
	}); err == nil {
		t.Error("an ordinary subscriber published a story as the channel")
	}

	if _, err := service.Create(ctx, stories.CreateInput{
		AuthorID: owner, ChannelChatID: &channelID,
		Type: "text", Caption: "from the newsroom",
	}); err != nil {
		t.Errorf("the owner could not publish as their own channel: %v", err)
	}
}

func TestAPersonalStoryNeedsNoChannelPermission(t *testing.T) {
	h := newCommentHarness(t)

	author := createUser(t, h.db, "author")
	service := stories.NewService(stories.NewRepository(h.db), h.messaging)

	// The check is about the channel field only; an ordinary story is
	// untouched by it.
	if _, err := service.Create(context.Background(), stories.CreateInput{
		AuthorID: author, Type: "text", Caption: "just me",
	}); err != nil {
		t.Errorf("an ordinary story was refused: %v", err)
	}
}

func TestAGroupCannotPublishStories(t *testing.T) {
	h := newCommentHarness(t)

	owner := createUser(t, h.db, "owner")
	groupID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	service := stories.NewService(stories.NewRepository(h.db), h.messaging)

	if _, err := service.Create(context.Background(), stories.CreateInput{
		AuthorID: owner, ChannelChatID: &groupID,
		Type: "text", Caption: "from a group",
	}); err == nil {
		t.Error("a group published a story as though it were a channel")
	}
}
