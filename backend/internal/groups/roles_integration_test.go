package groups_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Named permission bundles (§14).
//
// The claims worth pinning down: a bundle takes effect for everyone holding
// it, editing it reaches all of them at once, deleting it costs a membership
// nothing, and nobody can mint a role carrying more than they hold themselves.

func TestARoleGrantsItsPermissionsToWhoeverHoldsIt(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// An ordinary member cannot pin.
	before, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if before.Permissions.PinMessages {
		t.Fatal("an ordinary member can already pin; the test proves nothing")
	}

	role, err := h.service.CreateRole(ctx, chatID, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.service.AssignRole(ctx, chatID, member, owner, &role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	after, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !after.Permissions.PinMessages {
		t.Error("the bundle granted nothing")
	}
	if after.CustomRole != "Curator" {
		t.Errorf("custom role = %q, want Curator", after.CustomRole)
	}
	// The built-in role is untouched: a bundle changes what someone may do,
	// not where they sit.
	if after.Role != messaging.RoleMember {
		t.Errorf("built-in role = %q; a bundle must not promote anybody", after.Role)
	}
}

func TestEditingARoleReachesEveryoneHoldingIt(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	first := createUser(t, h.db, "first")
	second := createUser(t, h.db, "second")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	for _, member := range []uuid.UUID{first, second} {
		if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}

	role, err := h.service.CreateRole(ctx, chatID, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	for _, member := range []uuid.UUID{first, second} {
		if err := h.service.AssignRole(ctx, chatID, member, owner, &role.ID); err != nil {
			t.Fatalf("AssignRole: %v", err)
		}
	}

	// Correcting the bundle should not mean visiting each member — that is the
	// reason to have named roles at all.
	if _, err := h.service.UpdateRole(ctx, chatID, role.ID, owner, "Curator",
		map[string]bool{"pin_messages": true, "delete_messages": true}, 10); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}

	for _, member := range []uuid.UUID{first, second} {
		chatCtx, err := h.messaging.ChatContextFor(ctx, chatID, member)
		if err != nil {
			t.Fatalf("ChatContextFor: %v", err)
		}
		if !chatCtx.Permissions.DeleteMessages {
			t.Errorf("a holder did not gain the permission added to the bundle")
		}
	}
}

func TestAPersonalOverrideStillBeatsTheBundle(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	role, err := h.service.CreateRole(ctx, chatID, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.service.AssignRole(ctx, chatID, member, owner, &role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	// The bundle is handed to several people; an exception granted to one of
	// them individually has to win, or there is no way to make one.
	if err := h.repo.SetRole(ctx, chatID, member, messaging.RoleMember,
		map[string]bool{"pin_messages": false}, ""); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	chatCtx, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if chatCtx.Permissions.PinMessages {
		t.Error("the bundle overrode a personal exception")
	}
}

func TestDeletingARoleCostsTheHolderTheirGrantAndNotTheirPlace(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	role, err := h.service.CreateRole(ctx, chatID, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.service.AssignRole(ctx, chatID, member, owner, &role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := h.service.DeleteRole(ctx, chatID, role.ID, owner); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}

	chatCtx, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !chatCtx.IsMember {
		t.Fatal("deleting a role removed the person who held it from the chat")
	}
	if chatCtx.Permissions.PinMessages {
		t.Error("the grant survived the role it came from")
	}
}

func TestNobodyMayDefineARoleCarryingMoreThanTheyHold(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	moderator := createUser(t, h.db, "moderator")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, moderator, messaging.RoleAdmin, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	// An admin may manage admins but is not the owner, and does not hold
	// manage_admins... they do. Take it away explicitly so the test is about
	// the rule rather than about the default.
	if err := h.repo.SetRole(ctx, chatID, moderator, messaging.RoleAdmin,
		map[string]bool{"manage_admins": true, "ban_members": false}, ""); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	// Defining a role is granting it, just to more people at once.
	if _, err := h.service.CreateRole(ctx, chatID, moderator, "Enforcer",
		map[string]bool{"ban_members": true}, 5); err == nil {
		t.Error("a bundle was defined carrying a permission its author does not hold")
	}

	if _, err := h.service.CreateRole(ctx, chatID, owner, "Enforcer",
		map[string]bool{"ban_members": true}, 5); err != nil {
		t.Errorf("the owner could not define a role they hold: %v", err)
	}
}

func TestARoleFromAnotherChatCannotBeAssigned(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	here := h.chat(t, messaging.ChatGroup, "Here", owner, false)
	elsewhere := h.chat(t, messaging.ChatGroup, "Elsewhere", owner, false)
	if err := h.repo.AddMember(ctx, here, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	foreign, err := h.service.CreateRole(ctx, elsewhere, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	// Give the member a role of this chat first, so what happens to it can be
	// observed. This is the case that matters: a refusal that also wipes an
	// existing grant is a demotion nobody asked for.
	local, err := h.service.CreateRole(ctx, here, owner, "Moderator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.service.AssignRole(ctx, here, member, owner, &local.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	// The foreign key alone would accept this and quietly grant another chat's
	// permissions here. The scoping subquery stops the grant — but on its own
	// it made the update write NULL and report success, so the caller was told
	// the assignment worked while the member silently lost the role they had.
	err = h.service.AssignRole(ctx, here, member, owner, &foreign.ID)
	if err == nil {
		t.Fatal("assigning another chat's role was accepted")
	}
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("want a 404, got %v", err)
	}

	chatCtx, err := h.messaging.ChatContextFor(ctx, here, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if chatCtx.CustomRole != "Moderator" || !chatCtx.Permissions.PinMessages {
		t.Errorf("the refusal took away the role the member already held: %q",
			chatCtx.CustomRole)
	}
}

func TestARoleCannotNameAnUnknownPermission(t *testing.T) {
	h := newCommentHarness(t)

	owner := createUser(t, h.db, "owner")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)

	if _, err := h.service.CreateRole(context.Background(), chatID, owner, "Curator",
		map[string]bool{"pin_message": true}, 10); err == nil {
		t.Error("a bundle named a permission that does not exist")
	}
}

func TestRolesAreListedWithHowManyHoldThem(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	role, err := h.service.CreateRole(ctx, chatID, owner, "Curator",
		map[string]bool{"pin_messages": true}, 10)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.service.AssignRole(ctx, chatID, member, owner, &role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	roles, err := h.service.Roles(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("Roles: %v", err)
	}
	if len(roles) != 1 || roles[0].MemberCount != 1 {
		t.Errorf("roles = %+v; want one role held by one person", roles)
	}
}
