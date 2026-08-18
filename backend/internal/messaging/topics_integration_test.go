package messaging_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/messaging"
)

// Forum topics (§14).
//
// The claims worth pinning down are that converting a group never orphans its
// history, that every message in a forum belongs to exactly one topic, and
// that closing or deleting a topic is moderation rather than something any
// member can do to a conversation other people are having.

func sendToTopic(t *testing.T, service *messaging.Service, chatID, senderID uuid.UUID, topicID *uuid.UUID, content string) *messaging.Message {
	t.Helper()
	message, err := service.Send(context.Background(), messaging.SendInput{
		ChatID: chatID, SenderID: senderID, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: content, TopicID: topicID,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return message
}

func topicByID(t *testing.T, service *messaging.Service, chatID, viewerID, topicID uuid.UUID) messaging.Topic {
	t.Helper()
	topics, err := service.Topics(context.Background(), chatID, viewerID, 100)
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	for _, topic := range topics {
		if topic.ID == topicID {
			return topic
		}
	}
	t.Fatalf("topic %s is not in the list", topicID)
	return messaging.Topic{}
}

func TestConvertingAGroupFilesItsHistoryUnderGeneral(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	before := send(t, repo, chatID, member, "said before the conversion")

	general, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	if !general.IsGeneral {
		t.Error("the topic created by the conversion is not the General topic")
	}

	// The old message must be reachable from a client that only ever opens
	// topics, which means it has to belong to one.
	filed, err := service.TopicHistory(ctx, chatID, general.ID, owner, nil, nil, 50)
	if err != nil {
		t.Fatalf("TopicHistory: %v", err)
	}
	found := false
	for _, message := range filed {
		if message.ID == before.ID {
			found = true
		}
	}
	if !found {
		t.Error("the group's existing history is not in any topic; it is unreachable")
	}
}

func TestEnablingAForumTwiceReturnsTheSameGeneralTopic(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)

	first, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	second, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum (again): %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("a retried conversion made a second General topic (%s and %s)", first.ID, second.ID)
	}
}

func TestAMessageInAForumAlwaysBelongsToATopic(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)
	general, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}

	// A client that has never heard of topics sends without one.
	message := sendToTopic(t, service, chatID, owner, nil, "no topic named")
	if message.TopicID == nil {
		t.Fatal("a message in a forum belongs to no topic; it would be invisible in a topic-first client")
	}
	if *message.TopicID != general.ID {
		t.Errorf("it was filed under %s, want General %s", *message.TopicID, general.ID)
	}
}

func TestATopicIdOutsideAForumIsDropped(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	forum := groupChat(t, db, owner)
	plain := groupChat(t, db, owner)

	general, err := service.EnableForum(ctx, forum, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}

	// A stale client naming a topic from another group must not be able to
	// file messages into a group that has no topics.
	message := sendToTopic(t, service, plain, owner, &general.ID, "wrong chat")
	if message.TopicID != nil {
		t.Errorf("a message in a plain group was filed under topic %s", *message.TopicID)
	}
}

func TestMessagesAreSeparatedByTopic(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)
	general, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	other, err := service.CreateTopic(ctx, chatID, owner, "Bugs", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	inGeneral := sendToTopic(t, service, chatID, owner, &general.ID, "general chatter")
	inOther := sendToTopic(t, service, chatID, owner, &other.ID, "a bug report")

	generalMessages, err := service.TopicHistory(ctx, chatID, general.ID, owner, nil, nil, 50)
	if err != nil {
		t.Fatalf("TopicHistory: %v", err)
	}
	for _, message := range generalMessages {
		if message.ID == inOther.ID {
			t.Error("a message from another topic showed up in General")
		}
	}

	otherMessages, err := service.TopicHistory(ctx, chatID, other.ID, owner, nil, nil, 50)
	if err != nil {
		t.Fatalf("TopicHistory: %v", err)
	}
	if len(otherMessages) != 1 || otherMessages[0].ID != inOther.ID {
		t.Errorf("the topic holds %d messages, want just its own", len(otherMessages))
	}

	// The chat's own history still holds everything: a topic is a partition of
	// the messages, not a separate chat.
	whole, err := service.History(ctx, chatID, owner, nil, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(whole) < 2 {
		t.Errorf("the chat history holds %d messages, want both", len(whole))
	}
	_ = inGeneral
}

func TestTheTopicCountsFollowTheMessages(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Releases", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// The member has to have a cursor row before anything can be counted
	// against it, which is what opening the topic does.
	if _, err := service.MarkTopicRead(ctx, chatID, topic.ID, member, 0); err != nil {
		t.Fatalf("MarkTopicRead: %v", err)
	}

	sendToTopic(t, service, chatID, owner, &topic.ID, "one")
	sendToTopic(t, service, chatID, owner, &topic.ID, "two")

	fromMember := topicByID(t, service, chatID, member, topic.ID)
	if fromMember.MessageCount != 2 {
		t.Errorf("message count = %d, want 2", fromMember.MessageCount)
	}
	if fromMember.UnreadCount != 2 {
		t.Errorf("unread = %d, want 2 — a forum member follows some topics and ignores others",
			fromMember.UnreadCount)
	}

	// The sender's own count stays at nothing: a message you sent is not
	// unread for you.
	fromOwner := topicByID(t, service, chatID, owner, topic.ID)
	if fromOwner.UnreadCount != 0 {
		t.Errorf("the sender has %d unread in their own topic", fromOwner.UnreadCount)
	}
}

func TestReadingATopicClearsItsOwnCount(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Releases", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if _, err := service.MarkTopicRead(ctx, chatID, topic.ID, member, 0); err != nil {
		t.Fatalf("MarkTopicRead: %v", err)
	}

	last := sendToTopic(t, service, chatID, owner, &topic.ID, "one")
	if _, err := service.MarkTopicRead(ctx, chatID, topic.ID, member, last.Seq); err != nil {
		t.Fatalf("MarkTopicRead: %v", err)
	}

	if got := topicByID(t, service, chatID, member, topic.ID); got.UnreadCount != 0 {
		t.Errorf("unread = %d after reading the topic", got.UnreadCount)
	}
}

func TestAClosedTopicIsClosedToMembersButNotToStaff(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Settled", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	closed := true
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, owner,
		messaging.TopicUpdate{IsClosed: &closed}); err != nil {
		t.Fatalf("UpdateTopic: %v", err)
	}

	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: member, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "one more thing", TopicID: &topic.ID,
	}); err == nil {
		t.Error("a member posted into a closed topic")
	}

	// The staff who closed it still need to be able to say why.
	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: owner, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "closing this, see the other thread", TopicID: &topic.ID,
	}); err != nil {
		t.Errorf("the owner could not post into the topic they closed: %v", err)
	}
}

func TestClosingATopicIsModeration(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}

	// The member opens their own topic, so authorship is not the question —
	// closing is.
	topic, err := service.CreateTopic(ctx, chatID, member, "My question", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	closed := true
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, member,
		messaging.TopicUpdate{IsClosed: &closed}); err == nil {
		t.Error("an ordinary member closed a topic; that ends a conversation other people are having")
	}

	// Renaming their own topic is theirs to do.
	title := "My question, restated"
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, member,
		messaging.TopicUpdate{Title: &title}); err != nil {
		t.Errorf("the author could not rename their own topic: %v", err)
	}
}

func TestRenamingSomebodyElsesTopicNeedsThePermission(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	author := createUser(t, db, "author")
	stranger := createUser(t, db, "stranger")
	chatID := groupChat(t, db, owner, author, stranger)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, author, "Theirs", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	title := "Renamed by a stranger"
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, stranger,
		messaging.TopicUpdate{Title: &title}); err == nil {
		t.Error("an unrelated member renamed somebody else's topic")
	}
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, owner,
		messaging.TopicUpdate{Title: &title}); err != nil {
		t.Errorf("the owner could not rename a topic: %v", err)
	}
}

func TestTheGeneralTopicCannotBeDeleted(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)
	general, err := service.EnableForum(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("EnableForum: %v", err)
	}

	if err := service.DeleteTopic(ctx, chatID, general.ID, owner); err == nil {
		t.Error("the General topic was deleted; it holds the group's converted history")
	}
}

func TestDeletingATopicTakesItsMessagesWithIt(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Doomed", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	message := sendToTopic(t, service, chatID, owner, &topic.ID, "said in a doomed topic")

	if err := service.DeleteTopic(ctx, chatID, topic.ID, owner); err != nil {
		t.Fatalf("DeleteTopic: %v", err)
	}

	var deleted bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT deleted_at IS NOT NULL FROM messages WHERE id = $1`, message.ID).Scan(&deleted); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if !deleted {
		t.Error("a message survived its topic; it is in a forum with no thread to open it from")
	}

	topics, err := service.Topics(ctx, chatID, owner, 100)
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	for _, remaining := range topics {
		if remaining.ID == topic.ID {
			t.Error("the deleted topic is still listed")
		}
	}
}

func TestHiddenTopicsAreForStaffOnly(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Hidden", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	hidden := true
	if _, err := service.UpdateTopic(ctx, chatID, topic.ID, owner,
		messaging.TopicUpdate{IsHidden: &hidden}); err != nil {
		t.Fatalf("UpdateTopic: %v", err)
	}

	memberTopics, err := service.Topics(ctx, chatID, member, 100)
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	for _, listed := range memberTopics {
		if listed.ID == topic.ID {
			t.Error("a hidden topic is visible to an ordinary member")
		}
	}

	// Staff need to see what they hid, or it is unrecoverable.
	staffTopics, err := service.Topics(ctx, chatID, owner, 100)
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	found := false
	for _, listed := range staffTopics {
		if listed.ID == topic.ID {
			found = true
		}
	}
	if !found {
		t.Error("staff cannot see the topic they hid")
	}
}

func TestOnlyMembersSeeAForumsTopics(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}

	if _, err := service.Topics(ctx, chatID, outsider, 100); err == nil {
		t.Error("an outsider listed a forum's topics")
	}
}

func TestOnlyAGroupCanBecomeAForum(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	friend := createUser(t, db, "friend")
	chatID := privateChat(t, db, owner, friend)

	if _, err := service.EnableForum(ctx, chatID, owner); err == nil {
		t.Error("a one-to-one chat became a forum")
	}
}

func TestTurningForumModeOffKeepsTheFiling(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	topic, err := service.CreateTopic(ctx, chatID, owner, "Kept", "", 0)
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	sendToTopic(t, service, chatID, owner, &topic.ID, "filed here")

	if err := service.DisableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("DisableForum: %v", err)
	}
	// A message sent now belongs to no topic: the group is an ordinary group.
	plain := sendToTopic(t, service, chatID, owner, nil, "after")
	if plain.TopicID != nil {
		t.Errorf("a message in a plain group was filed under topic %s", *plain.TopicID)
	}

	// Switching back finds the filing where it was left rather than an empty
	// forum.
	if _, err := service.EnableForum(ctx, chatID, owner); err != nil {
		t.Fatalf("EnableForum: %v", err)
	}
	kept, err := service.TopicHistory(ctx, chatID, topic.ID, owner, nil, nil, 50)
	if err != nil {
		t.Fatalf("TopicHistory: %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("the topic holds %d messages after a round trip through plain mode, want 1", len(kept))
	}
}
