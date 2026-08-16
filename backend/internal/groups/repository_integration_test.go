package groups_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/polls"
	"github.com/sobh/messenger/backend/internal/stories"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}

	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 8, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func createUser(t *testing.T, db *database.DB, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		"+9891"+uuid.NewString()[:9], []byte(label)).Scan(&id)
	if err != nil {
		t.Fatalf("create user %s: %v", label, err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`, id, label); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_event_counters (user_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("create event counter: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestCreateGroupSeedsOwnerAndSettings(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Team", "A team chat", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	messagingRepo := messaging.NewRepository(db)
	chatCtx, err := messagingRepo.ChatContextFor(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !chatCtx.IsMember || chatCtx.Role != messaging.RoleOwner {
		t.Errorf("creator role = %q, want owner", chatCtx.Role)
	}
	// The owner must come out of creation able to administer the chat.
	if !chatCtx.Permissions.ManageAdmins || !chatCtx.Permissions.EditGroup {
		t.Error("owner does not hold administrative permissions")
	}

	if _, err := repo.Settings(ctx, chatID); err != nil {
		t.Errorf("settings row was not created: %v", err)
	}
}

func TestMemberCountTracksJoinsAndLeaves(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	alice := createUser(t, db, "alice")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Counters", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if err := repo.AddMember(ctx, chatID, alice, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if got := memberCount(t, db, chatID); got != 2 {
		t.Errorf("member count after join = %d, want 2", got)
	}

	// Adding the same person twice must not inflate the count.
	if err := repo.AddMember(ctx, chatID, alice, messaging.RoleMember, &owner); err == nil {
		t.Error("expected a second AddMember to report the user is already a member")
	}
	if got := memberCount(t, db, chatID); got != 2 {
		t.Errorf("member count after duplicate join = %d, want 2", got)
	}

	if err := repo.RemoveMember(ctx, chatID, alice); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if got := memberCount(t, db, chatID); got != 1 {
		t.Errorf("member count after leave = %d, want 1", got)
	}
}

func TestMemberLimitIsEnforced(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Tiny", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := repo.UpdateSettings(ctx, chatID, groups.Settings{
		MaxMembers: 2, HistoryVisibleToNew: true,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	first := createUser(t, db, "first")
	if err := repo.AddMember(ctx, chatID, first, messaging.RoleMember, nil); err != nil {
		t.Fatalf("AddMember (within limit): %v", err)
	}

	second := createUser(t, db, "second")
	if err := repo.AddMember(ctx, chatID, second, messaging.RoleMember, nil); err == nil {
		t.Error("expected the member limit to reject the third member")
	}
}

func TestInviteLinkLifecycle(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Invites", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	limit := 5
	link, err := repo.CreateInviteLink(ctx, chatID, owner, "launch", &limit, nil)
	if err != nil {
		t.Fatalf("CreateInviteLink: %v", err)
	}
	if link.Slug == "" {
		t.Fatal("invite link has no slug")
	}

	resolved, err := repo.ResolveInvite(ctx, link.Slug)
	if err != nil {
		t.Fatalf("ResolveInvite: %v", err)
	}
	if resolved.ChatID != chatID {
		t.Errorf("resolved chat = %s, want %s", resolved.ChatID, chatID)
	}

	if err := repo.RevokeInviteLink(ctx, chatID, link.ID); err != nil {
		t.Fatalf("RevokeInviteLink: %v", err)
	}
	// A revoked link must stop working immediately.
	if _, err := repo.ResolveInvite(ctx, link.Slug); err == nil {
		t.Error("a revoked invite link still resolves")
	}
}

func TestJoinRequestApprovalAddsMember(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	applicant := createUser(t, db, "applicant")
	chatID, err := repo.CreateChat(ctx, messaging.ChatGroup, "Gated", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if err := repo.CreateJoinRequest(ctx, chatID, applicant, nil); err != nil {
		t.Fatalf("CreateJoinRequest: %v", err)
	}

	requests, err := repo.JoinRequests(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("JoinRequests: %v", err)
	}
	if len(requests) != 1 || requests[0].UserID != applicant {
		t.Fatalf("join requests = %+v, want one from the applicant", requests)
	}

	if err := repo.ResolveJoinRequest(ctx, chatID, applicant, owner, true); err != nil {
		t.Fatalf("ResolveJoinRequest: %v", err)
	}
	if got := memberCount(t, db, chatID); got != 2 {
		t.Errorf("member count after approval = %d, want 2", got)
	}

	// The queue must be empty once the request is resolved.
	requests, err = repo.JoinRequests(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("JoinRequests after approval: %v", err)
	}
	if len(requests) != 0 {
		t.Errorf("%d requests still pending after approval", len(requests))
	}
}

func TestChannelViewsCountDistinctViewers(t *testing.T) {
	db := testDB(t)
	repo := groups.NewRepository(db)
	messagingRepo := messaging.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	reader := createUser(t, db, "reader")
	chatID, err := repo.CreateChat(ctx, messaging.ChatChannel, "News", "", owner, true,
		strPtr("chan"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := repo.AddMember(ctx, chatID, reader, messaging.RoleMember, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	sent, err := messagingRepo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: owner, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "first post",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Two views by the same reader count once.
	for i := 0; i < 2; i++ {
		if err := repo.RecordPostView(ctx, sent.Message.ID, chatID, reader); err != nil {
			t.Fatalf("RecordPostView: %v", err)
		}
	}

	stats, err := repo.PostStats(ctx, chatID, []uuid.UUID{sent.Message.ID})
	if err != nil {
		t.Fatalf("PostStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d stat rows, want 1", len(stats))
	}
	if stats[0].ViewCount != 1 {
		t.Errorf("view count = %d after two views by one reader, want 1", stats[0].ViewCount)
	}
}

func TestPollVotingMovesTheVoteRatherThanAddingOne(t *testing.T) {
	db := testDB(t)
	groupsRepo := groups.NewRepository(db)
	pollsRepo := polls.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	voter := createUser(t, db, "voter")
	chatID, err := groupsRepo.CreateChat(ctx, messaging.ChatGroup, "Polls", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := groupsRepo.AddMember(ctx, chatID, voter, messaging.RoleMember, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	poll, err := pollsRepo.Create(ctx, polls.CreateInput{
		ChatID:    chatID,
		CreatedBy: owner,
		Question:  "Tea or coffee?",
		Options:   []string{"Tea", "Coffee"},
	})
	if err != nil {
		t.Fatalf("Create poll: %v", err)
	}
	if len(poll.Options) != 2 {
		t.Fatalf("poll has %d options, want 2", len(poll.Options))
	}

	if err := pollsRepo.Vote(ctx, poll.ID, voter, []uuid.UUID{poll.Options[0].ID}); err != nil {
		t.Fatalf("Vote: %v", err)
	}

	// Changing a single-choice vote must move it, not add a second.
	if err := pollsRepo.Vote(ctx, poll.ID, voter, []uuid.UUID{poll.Options[1].ID}); err != nil {
		t.Fatalf("Vote (change): %v", err)
	}

	updated, err := pollsRepo.ByID(ctx, poll.ID, voter)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if updated.Options[0].VoteCount != 0 {
		t.Errorf("first option still has %d votes after the voter changed their mind",
			updated.Options[0].VoteCount)
	}
	if updated.Options[1].VoteCount != 1 {
		t.Errorf("second option has %d votes, want 1", updated.Options[1].VoteCount)
	}
	if updated.TotalVoters != 1 {
		t.Errorf("total voters = %d, want 1", updated.TotalVoters)
	}
	if len(updated.MyVotes) != 1 || updated.MyVotes[0] != poll.Options[1].ID {
		t.Errorf("my votes = %v, want only the second option", updated.MyVotes)
	}
}

func TestSingleChoicePollRejectsMultipleAnswers(t *testing.T) {
	db := testDB(t)
	groupsRepo := groups.NewRepository(db)
	pollsRepo := polls.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID, err := groupsRepo.CreateChat(ctx, messaging.ChatGroup, "Polls", "", owner, false, nil)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	poll, err := pollsRepo.Create(ctx, polls.CreateInput{
		ChatID: chatID, CreatedBy: owner,
		Question: "Pick one", Options: []string{"A", "B"},
	})
	if err != nil {
		t.Fatalf("Create poll: %v", err)
	}

	err = pollsRepo.Vote(ctx, poll.ID, owner,
		[]uuid.UUID{poll.Options[0].ID, poll.Options[1].ID})
	if err == nil {
		t.Error("a single-choice poll accepted two answers")
	}
}

// A story shared with contacts must not appear to a stranger.
func TestStoryPrivacyExcludesNonContacts(t *testing.T) {
	db := testDB(t)
	repo := stories.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	contact := createUser(t, db, "contact")
	stranger := createUser(t, db, "stranger")

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO contacts (owner_id, contact_id) VALUES ($1, $2)`, author, contact); err != nil {
		t.Fatalf("create contact: %v", err)
	}

	story, err := repo.Create(ctx, stories.CreateInput{
		AuthorID: author,
		Type:     "text",
		Caption:  "contacts only",
		Privacy:  stories.PrivacyContacts,
	})
	if err != nil {
		t.Fatalf("Create story: %v", err)
	}

	if !feedContains(t, repo, contact, story.ID) {
		t.Error("the author's contact cannot see a contacts-only story")
	}
	if feedContains(t, repo, stranger, story.ID) {
		t.Error("a stranger can see a contacts-only story")
	}

	allowed, err := repo.CanView(ctx, story.ID, stranger)
	if err != nil {
		t.Fatalf("CanView: %v", err)
	}
	if allowed {
		t.Error("CanView allowed a stranger to view a contacts-only story")
	}
}

func TestStoryDenyListOverridesPublicPrivacy(t *testing.T) {
	db := testDB(t)
	repo := stories.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	blocked := createUser(t, db, "blocked")

	story, err := repo.Create(ctx, stories.CreateInput{
		AuthorID: author,
		Type:     "text",
		Caption:  "public but not for everyone",
		Privacy:  stories.PrivacyEveryone,
		DenyList: []uuid.UUID{blocked},
	})
	if err != nil {
		t.Fatalf("Create story: %v", err)
	}

	if feedContains(t, repo, blocked, story.ID) {
		t.Error("a denied viewer can still see a public story")
	}
}

func TestStoryViewsCountOnce(t *testing.T) {
	db := testDB(t)
	repo := stories.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	viewer := createUser(t, db, "viewer")

	story, err := repo.Create(ctx, stories.CreateInput{
		AuthorID: author, Type: "text", Caption: "hello", Privacy: stories.PrivacyEveryone,
	})
	if err != nil {
		t.Fatalf("Create story: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := repo.RecordView(ctx, story.ID, viewer, nil); err != nil {
			t.Fatalf("RecordView: %v", err)
		}
	}

	viewers, err := repo.Viewers(ctx, story.ID, author, 10)
	if err != nil {
		t.Fatalf("Viewers: %v", err)
	}
	if len(viewers) != 1 {
		t.Errorf("got %d viewers after three views by one person, want 1", len(viewers))
	}

	var viewCount int
	if err := db.Pool.QueryRow(ctx,
		`SELECT view_count FROM stories WHERE id = $1`, story.ID).Scan(&viewCount); err != nil {
		t.Fatalf("read view count: %v", err)
	}
	if viewCount != 1 {
		t.Errorf("view count = %d, want 1", viewCount)
	}
}

func feedContains(t *testing.T, repo *stories.Repository, viewer, storyID uuid.UUID) bool {
	t.Helper()

	feed, err := repo.Feed(context.Background(), viewer, 100)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	for _, story := range feed {
		if story.ID == storyID {
			return true
		}
	}
	return false
}

func memberCount(t *testing.T, db *database.DB, chatID uuid.UUID) int {
	t.Helper()

	var count int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT member_count FROM chats WHERE id = $1`, chatID).Scan(&count); err != nil {
		t.Fatalf("read member count: %v", err)
	}
	return count
}

func strPtr(s string) *string { return &s }
