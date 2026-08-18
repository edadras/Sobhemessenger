package news_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/news"
)

// The news feed (§28).
//
// Every mode is exercised against real PostgreSQL because the failure this
// suite exists for is not a wrong answer but a rejected query: the feed builds
// its SQL and its argument list separately, and any drift between the two is
// invisible until a request actually runs. Compiling proves nothing here.

func testDB(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}

	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func createViewer(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		"+9891"+uuid.NewString()[:9], []byte(uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// publishArticle inserts a published article and returns its id and category.
func publishArticle(t *testing.T, db *database.DB, title string, breaking bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var categoryID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO news_categories (slug) VALUES ($1) RETURNING id`,
		"cat-"+uuid.NewString()[:8]).Scan(&categoryID); err != nil {
		t.Fatalf("create category: %v", err)
	}

	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO news_articles (slug, locale, title, status, category_id, is_breaking, published_at)
		VALUES ($1, 'fa', $2, 'published', $3, $4, now()) RETURNING id`,
		"slug-"+uuid.NewString()[:8], title, categoryID, breaking).Scan(&id); err != nil {
		t.Fatalf("create article: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM news_articles WHERE id = $1`, id)
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM news_categories WHERE id = $1`, categoryID)
	})
	return id, categoryID
}

func TestEveryFeedModeRuns(t *testing.T) {
	// The bug this catches shipped: the query named $1 to $4 in the ordinary
	// case but always passed six arguments, so PostgreSQL refused it with
	// "expected 4 arguments, got 6" and the whole news tab returned 500. It
	// only appeared when a request actually reached the database.
	db := testDB(t)
	repo := news.NewRepository(db)
	ctx := context.Background()

	viewer := createViewer(t, db)
	_, categoryID := publishArticle(t, db, "خبر آزمایشی", false)
	publishArticle(t, db, "خبر فوری", true)

	before := time.Now().Add(time.Hour)

	cases := []struct {
		name  string
		query news.FeedQuery
	}{
		{"latest", news.FeedQuery{Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 20}},
		{"popular", news.FeedQuery{Mode: "popular", Locale: "fa", ViewerID: viewer, Limit: 20}},
		{"following", news.FeedQuery{Mode: "following", Locale: "fa", ViewerID: viewer, Limit: 20}},
		{"breaking", news.FeedQuery{Mode: "breaking", Locale: "fa", ViewerID: viewer, Limit: 20}},
		{"an unknown mode falls back", news.FeedQuery{Mode: "nonsense", Locale: "fa", ViewerID: viewer, Limit: 20}},
		{"by category", news.FeedQuery{Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 20, CategoryID: &categoryID}},
		{"by tag", news.FeedQuery{Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 20, Tag: "politics"}},
		{"category and tag together", news.FeedQuery{
			Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 20,
			CategoryID: &categoryID, Tag: "politics",
		}},
		{"paged", news.FeedQuery{Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 20, Before: &before}},
		{"popular by category", news.FeedQuery{
			Mode: "popular", Locale: "fa", ViewerID: viewer, Limit: 20, CategoryID: &categoryID,
		}},
		{"following by category", news.FeedQuery{
			Mode: "following", Locale: "fa", ViewerID: viewer, Limit: 20, CategoryID: &categoryID,
		}},
		{"breaking with both filters", news.FeedQuery{
			Mode: "breaking", Locale: "fa", ViewerID: viewer, Limit: 20,
			CategoryID: &categoryID, Tag: "politics",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := repo.Feed(ctx, tc.query); err != nil {
				t.Fatalf("Feed: %v", err)
			}
		})
	}
}

func TestTheFeedReturnsPublishedArticles(t *testing.T) {
	// A query that runs but returns nothing would satisfy the test above while
	// leaving the feed empty, so this checks it actually finds an article.
	db := testDB(t)
	repo := news.NewRepository(db)

	viewer := createViewer(t, db)
	id, _ := publishArticle(t, db, "خبر آزمایشی", false)

	articles, err := repo.Feed(context.Background(), news.FeedQuery{
		Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 50,
	})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}

	found := false
	for _, article := range articles {
		if article.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("the published article was not in the feed of %d", len(articles))
	}
}

func TestDraftsStayOutOfTheFeed(t *testing.T) {
	db := testDB(t)
	repo := news.NewRepository(db)
	ctx := context.Background()

	viewer := createViewer(t, db)

	var draftID uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO news_articles (slug, locale, title, status)
		VALUES ($1, 'fa', 'پیش‌نویس', 'draft') RETURNING id`,
		"draft-"+uuid.NewString()[:8]).Scan(&draftID); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM news_articles WHERE id = $1`, draftID)
	})

	articles, err := repo.Feed(ctx, news.FeedQuery{
		Mode: "latest", Locale: "fa", ViewerID: viewer, Limit: 50,
	})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	for _, article := range articles {
		if article.ID == draftID {
			t.Error("an unpublished draft appeared in the public feed")
		}
	}
}
