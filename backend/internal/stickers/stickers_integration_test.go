package stickers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/stickers"
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

func createUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte(uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// createMedia inserts a real media row, because a sticker's media_id is a
// foreign key: a set built on media that does not exist must be refused.
func createMedia(t *testing.T, db *database.DB, owner uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO media (owner_id, bucket, object_key, kind, mime_type, size_bytes)
		VALUES ($1, 'media', $2, 'image', 'image/webp', 4096)
		RETURNING id`, owner, "stickers/"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("create media: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM media WHERE id = $1`, id)
	})
	return id
}

func uniqueSlug(t *testing.T, db *database.DB) string {
	t.Helper()
	slug := "s" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM sticker_sets WHERE slug = $1`, slug)
	})
	return slug
}

func newSet(t *testing.T, svc *stickers.Service, db *database.DB, owner uuid.UUID, count int) *stickers.Set {
	t.Helper()

	pack := make([]stickers.Sticker, 0, count)
	for i := 0; i < count; i++ {
		pack = append(pack, stickers.Sticker{
			MediaID: createMedia(t, db, owner),
			Emoji:   []string{"😀", "😅", "🙂", "😎", "🤔"}[i%5],
		})
	}

	set, err := svc.CreateSet(context.Background(), owner,
		uniqueSlug(t, db), "بسته آزمایشی", "static", pack)
	if err != nil {
		t.Fatalf("CreateSet: %v", err)
	}
	return set
}

// ------------------------------------------------------------ sticker sets

func TestCreatingASetStoresItsStickersInOrder(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)

	set := newSet(t, svc, db, owner, 3)
	if len(set.Stickers) != 3 {
		t.Fatalf("the new set holds %d stickers, want 3", len(set.Stickers))
	}

	read, err := svc.BySlug(ctx, set.Slug, owner)
	if err != nil {
		t.Fatalf("BySlug: %v", err)
	}
	if len(read.Stickers) != 3 {
		t.Fatalf("the stored set holds %d stickers, want 3", len(read.Stickers))
	}
	// Position is what the picker draws by, so it must survive the round trip
	// and match the order the set was submitted in.
	for i, sticker := range read.Stickers {
		if sticker.Position != i {
			t.Errorf("sticker %d is at position %d", i, sticker.Position)
		}
		if sticker.MediaID != set.Stickers[i].MediaID {
			t.Errorf("sticker %d refers to %s, want %s", i, sticker.MediaID, set.Stickers[i].MediaID)
		}
	}
}

func TestSetSlugIsClaimedOnce(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()

	first := createUser(t, db)
	second := createUser(t, db)
	slug := uniqueSlug(t, db)

	pack := []stickers.Sticker{{MediaID: createMedia(t, db, first), Emoji: "😀"}}
	if _, err := svc.CreateSet(ctx, first, slug, "اولی", "static", pack); err != nil {
		t.Fatalf("first CreateSet: %v", err)
	}

	other := []stickers.Sticker{{MediaID: createMedia(t, db, second), Emoji: "😀"}}
	if _, err := svc.CreateSet(ctx, second, slug, "دومی", "static", other); err == nil {
		t.Fatal("two sets were created with the same slug, so a share link is ambiguous")
	}
}

// A slug appears in a share link, so it is held to the same alphabet as a
// username: no homoglyphs, no characters that need escaping.
func TestSetSlugAndContentsAreValidated(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)

	valid := []stickers.Sticker{{MediaID: createMedia(t, db, owner), Emoji: "😀"}}

	for _, slug := range []string{"ab", "1leading", "has-a-hyphen", "has space", "Ünicode"} {
		if _, err := svc.CreateSet(ctx, owner, slug, "عنوان", "static", valid); err == nil {
			t.Errorf("CreateSet accepted the slug %q", slug)
		}
	}

	if _, err := svc.CreateSet(ctx, owner, uniqueSlug(t, db), "   ", "static", valid); err == nil {
		t.Error("a set was created with a blank title")
	}
	if _, err := svc.CreateSet(ctx, owner, uniqueSlug(t, db), "عنوان", "static", nil); err == nil {
		t.Error("an empty set was created")
	}

	tooMany := make([]stickers.Sticker, stickers.MaxStickersPerSet+1)
	for i := range tooMany {
		tooMany[i] = stickers.Sticker{MediaID: valid[0].MediaID, Emoji: "😀"}
	}
	if _, err := svc.CreateSet(ctx, owner, uniqueSlug(t, db), "عنوان", "static", tooMany); err == nil {
		t.Errorf("a set with %d stickers was accepted", len(tooMany))
	}
}

// A sticker's media_id is a foreign key. Naming media that does not exist must
// be a validation failure, not a 500 — and must leave no half-built set behind.
func TestASetOnMissingMediaIsRefusedWhole(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)
	slug := uniqueSlug(t, db)

	pack := []stickers.Sticker{
		{MediaID: createMedia(t, db, owner), Emoji: "😀"},
		{MediaID: uuid.New(), Emoji: "😅"}, // never uploaded
	}
	if _, err := svc.CreateSet(ctx, owner, slug, "ناقص", "static", pack); err == nil {
		t.Fatal("a set was created referring to media that does not exist")
	}

	// The whole thing is one transaction, so the first sticker must not have
	// survived the second one's failure.
	var orphans int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM sticker_sets WHERE slug = $1`, slug).Scan(&orphans); err != nil {
		t.Fatalf("count sets: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d half-built sets survive the failure", orphans)
	}
}

func TestInstallingAndRemovingASet(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()

	author := createUser(t, db)
	installer := createUser(t, db)
	set := newSet(t, svc, db, author, 2)

	before, err := svc.BySlug(ctx, set.Slug, installer)
	if err != nil {
		t.Fatalf("BySlug: %v", err)
	}
	if before.IsAdded {
		t.Fatal("a set nobody installed reports itself as installed")
	}

	if err := svc.Add(ctx, installer, set.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Installing twice is what a double tap does; it must not be an error and
	// must not install the set twice.
	if err := svc.Add(ctx, installer, set.ID); err != nil {
		t.Fatalf("Add again: %v", err)
	}

	added, err := svc.Added(ctx, installer)
	if err != nil {
		t.Fatalf("Added: %v", err)
	}
	if len(added) != 1 || added[0].ID != set.ID {
		t.Fatalf("the installer has %d sets, want exactly the one they added", len(added))
	}
	// The picker draws the images, so the listing has to carry them.
	if len(added[0].Stickers) != 2 {
		t.Errorf("the installed set lists %d stickers, want 2", len(added[0].Stickers))
	}

	after, err := svc.BySlug(ctx, set.Slug, installer)
	if err != nil {
		t.Fatalf("BySlug after adding: %v", err)
	}
	if !after.IsAdded {
		t.Error("an installed set does not report itself as installed")
	}

	if err := svc.Remove(ctx, installer, set.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := svc.Remove(ctx, installer, set.ID); err == nil {
		t.Error("removing a set that is not installed reported success")
	}

	remaining, err := svc.Added(ctx, installer)
	if err != nil {
		t.Fatalf("Added after removing: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d sets remain installed", len(remaining))
	}
}

func TestInstalledSetsKeepTheirOrder(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()

	author := createUser(t, db)
	installer := createUser(t, db)

	first := newSet(t, svc, db, author, 1)
	second := newSet(t, svc, db, author, 1)
	third := newSet(t, svc, db, author, 1)

	for _, set := range []*stickers.Set{first, second, third} {
		if err := svc.Add(ctx, installer, set.ID); err != nil {
			t.Fatalf("Add %s: %v", set.Slug, err)
		}
	}

	added, err := svc.Added(ctx, installer)
	if err != nil {
		t.Fatalf("Added: %v", err)
	}
	if len(added) != 3 {
		t.Fatalf("%d sets are installed, want 3", len(added))
	}
	for i, want := range []*stickers.Set{first, second, third} {
		if added[i].ID != want.ID {
			t.Errorf("position %d holds %s, want %s", i, added[i].Slug, want.Slug)
		}
	}
}

func TestInstallingASetThatDoesNotExistIsRefused(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()
	user := createUser(t, db)

	if err := svc.Add(ctx, user, uuid.New()); err == nil {
		t.Error("a set that does not exist was installed")
	}
	if _, err := svc.BySlug(ctx, "no_such_set_anywhere", user); err == nil {
		t.Error("a set that does not exist was read")
	}
}

func TestSearchFindsSetsByTitleSlugAndEmoji(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)

	// A distinctive emoji, so the assertion is about this set and not about
	// whatever else the shared table holds.
	marker := "🦔"
	slug := uniqueSlug(t, db)
	title := "گربه‌های بامزه " + uuid.NewString()[:8]
	if _, err := svc.CreateSet(ctx, owner, slug, title, "static", []stickers.Sticker{
		{MediaID: createMedia(t, db, owner), Emoji: marker},
	}); err != nil {
		t.Fatalf("CreateSet: %v", err)
	}

	contains := func(sets []stickers.Set, want string) bool {
		for _, set := range sets {
			if set.Slug == want {
				return true
			}
		}
		return false
	}

	bySlug, err := svc.Search(ctx, slug, 0)
	if err != nil {
		t.Fatalf("Search by slug: %v", err)
	}
	if !contains(bySlug, slug) {
		t.Error("searching for the slug did not find the set")
	}

	byTitle, err := svc.Search(ctx, title, 0)
	if err != nil {
		t.Fatalf("Search by title: %v", err)
	}
	if !contains(byTitle, slug) {
		t.Error("searching for the title did not find the set")
	}

	byEmoji, err := svc.Search(ctx, marker, 0)
	if err != nil {
		t.Fatalf("Search by emoji: %v", err)
	}
	if !contains(byEmoji, slug) {
		t.Error("searching for a sticker's emoji did not find its set")
	}

	// An empty result is a list, not null: a client must not have to handle
	// both shapes.
	none, err := svc.Search(ctx, "zzz_nothing_matches_this_zzz", 0)
	if err != nil {
		t.Fatalf("Search with no matches: %v", err)
	}
	if none == nil {
		t.Error("a search with no matches returned null rather than an empty list")
	}
}

// ------------------------------------------------------------ link previews

// Unfurling reaches out to a stranger's server on a user's say-so, so the
// address is the security boundary: loopback, link-local and private ranges
// must all be refused before a connection is made.
func TestPreviewRefusesAddressesInsideTheNetwork(t *testing.T) {
	db := testDB(t)
	svc := stickers.NewService(stickers.NewRepository(db))
	ctx := context.Background()

	// A server really is listening here, so a guard that did not work would
	// succeed rather than merely fail to connect.
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>internal service</title></head><body></body></html>`)
	}))
	defer local.Close()

	for _, target := range []string{
		local.URL,                        // loopback, and actually serving
		"http://127.0.0.1/",              // loopback by literal
		"http://[::1]/",                  // loopback over IPv6
		"http://169.254.169.254/latest/", // the cloud metadata endpoint
		"http://10.0.0.1/",               // private range
		"http://192.168.1.1/",            // private range
		"http://172.16.0.1/",             // private range
		"file:///etc/passwd",             // not http at all
		"ftp://example.com/",             // nor this
		"not a url",
	} {
		if _, err := svc.Preview(ctx, target); err == nil {
			t.Errorf("Preview(%q) succeeded; the address should have been refused", target)
		}
	}
}

// A failed unfurl is cached as a failure. Without that, a message carrying a
// dead link would re-fetch it for every recipient who opened the chat.
func TestAFailedUnfurlIsCachedAsAFailure(t *testing.T) {
	db := testDB(t)
	repo := stickers.NewRepository(db)
	ctx := context.Background()

	target := "https://example.invalid/" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM link_previews WHERE url = $1`, target)
	})

	if _, hit, err := repo.CachedPreview(ctx, target); err != nil {
		t.Fatalf("CachedPreview before: %v", err)
	} else if hit {
		t.Fatal("a link nobody has fetched is already cached")
	}

	if err := repo.StorePreview(ctx, &stickers.LinkPreview{URL: target}, true); err != nil {
		t.Fatalf("StorePreview: %v", err)
	}

	preview, hit, err := repo.CachedPreview(ctx, target)
	if err != nil {
		t.Fatalf("CachedPreview after: %v", err)
	}
	if !hit {
		t.Fatal("the cached failure was not a cache hit, so the link will be re-fetched")
	}
	if preview != nil {
		t.Fatal("a cached failure came back as a usable preview")
	}
}

func TestAStoredPreviewIsReadBackAndReplaced(t *testing.T) {
	db := testDB(t)
	repo := stickers.NewRepository(db)
	ctx := context.Background()

	target := "https://example.test/" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM link_previews WHERE url = $1`, target)
	})

	original := &stickers.LinkPreview{
		URL: target, SiteName: "نمونه", Title: "تیتر", Description: "توضیح",
	}
	if err := repo.StorePreview(ctx, original, false); err != nil {
		t.Fatalf("StorePreview: %v", err)
	}

	read, hit, err := repo.CachedPreview(ctx, target)
	if err != nil {
		t.Fatalf("CachedPreview: %v", err)
	}
	if !hit || read == nil {
		t.Fatal("the stored preview did not come back")
	}
	if read.Title != original.Title || read.SiteName != original.SiteName ||
		read.Description != original.Description {
		t.Fatalf("read back %+v, want %+v", read, original)
	}

	// Re-fetching a page whose metadata changed must replace the entry, not
	// add a second one keyed on the same URL.
	revised := &stickers.LinkPreview{URL: target, Title: "تیتر تازه"}
	if err := repo.StorePreview(ctx, revised, false); err != nil {
		t.Fatalf("StorePreview again: %v", err)
	}
	read, _, err = repo.CachedPreview(ctx, target)
	if err != nil {
		t.Fatalf("CachedPreview after replacing: %v", err)
	}
	if read.Title != revised.Title {
		t.Errorf("the cache still holds %q, want %q", read.Title, revised.Title)
	}

	var entries int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM link_previews WHERE url = $1`, target).Scan(&entries); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entries != 1 {
		t.Errorf("%d cache entries exist for one URL, want 1", entries)
	}
}

func TestAnExpiredPreviewIsNotACacheHit(t *testing.T) {
	db := testDB(t)
	repo := stickers.NewRepository(db)
	ctx := context.Background()

	target := "https://example.test/" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM link_previews WHERE url = $1`, target)
	})

	if err := repo.StorePreview(ctx, &stickers.LinkPreview{
		URL: target, Title: "کهنه",
	}, false); err != nil {
		t.Fatalf("StorePreview: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE link_previews SET expires_at = now() - interval '1 hour' WHERE url = $1`,
		target); err != nil {
		t.Fatalf("expire the entry: %v", err)
	}

	if _, hit, err := repo.CachedPreview(ctx, target); err != nil {
		t.Fatalf("CachedPreview: %v", err)
	} else if hit {
		t.Fatal("an expired entry was served from the cache")
	}
}
