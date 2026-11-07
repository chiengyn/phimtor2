package main

import (
	"context"
	"testing"
	"time"
)

// watchTestTMDBID marks the throwaway title these tests create, so cleanup can
// find it (deleting the title cascades its video and its watch_sessions).
const watchTestTMDBID = 999_999_013

// testWatchVideo creates a throwaway title + torrent source + video and returns
// (titleID, videoID).
func testWatchVideo(t *testing.T, store *Store) (int64, int64) {
	t.Helper()
	db := store.db
	res, err := db.Exec(`INSERT INTO titles (tmdb_id, type, title) VALUES (?, 'movie', 'go-test watch title')`, watchTestTMDBID)
	if err != nil {
		t.Fatalf("create title: %v", err)
	}
	titleID, _ := res.LastInsertId()
	const hash = "go00000000000000000000000000000000test13"
	res, err = db.Exec(`INSERT INTO torrent_sources (info_hash, magnet) VALUES (?, 'magnet:?xt=urn:btih:go-test')`, hash)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	sourceID, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO videos (source_id, title_id, name, resolution, file_index, file_path)
		VALUES (?, ?, 'go-test', '1080p', 0, 'go-test.mp4')`, sourceID, titleID)
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	videoID, _ := res.LastInsertId()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM titles WHERE tmdb_id = ? AND type = 'movie'`, watchTestTMDBID)
		db.Exec(`DELETE FROM torrent_sources WHERE info_hash = ?`, hash)
	})
	return titleID, videoID
}

// The flush is an upsert of cumulative totals: writing the same session twice,
// or an older snapshot after a newer one, leaves one row with the newest
// numbers. That idempotence is what makes a failed flush safe to retry.
func TestWatchSessionsUpsertIntegration(t *testing.T) {
	store := testStore(t)
	userID := testUserID(t, store)
	titleID, videoID := testWatchVideo(t, store)
	ctx := context.Background()

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	row := watchSessionRow{UserID: userID, SessionID: "go-test-session", VideoID: videoID, TitleID: titleID,
		Resolution: "1080p", Client: "web", StartedAt: start, LastSeen: start.Add(10 * time.Minute), Seconds: 600}
	newer := row
	newer.LastSeen, newer.Seconds = start.Add(20*time.Minute), 1200

	for _, batch := range [][]watchSessionRow{{row}, {newer}, {newer}, {row}} {
		if err := store.UpsertWatchSessions(ctx, batch); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	var n, seconds int
	var lastSeen, started time.Time
	if err := store.db.QueryRow(`SELECT COUNT(*), MAX(watched_seconds), MAX(last_seen_at), MIN(started_at)
		FROM watch_sessions WHERE user_id = ? AND video_id = ?`, userID, videoID).
		Scan(&n, &seconds, &lastSeen, &started); err != nil {
		t.Fatal(err)
	}
	if n != 1 || seconds != 1200 || !lastSeen.Equal(newer.LastSeen) || !started.Equal(start) {
		t.Errorf("got rows=%d seconds=%d last=%v start=%v, want 1 / 1200 / %v / %v",
			n, seconds, lastSeen, started, newer.LastSeen, start)
	}
}

// A session whose video was deleted mid-flight is skipped without taking the
// rest of its batch down with it.
func TestWatchSessionsSkipOrphansIntegration(t *testing.T) {
	store := testStore(t)
	userID := testUserID(t, store)
	titleID, videoID := testWatchVideo(t, store)
	now := time.Now().Truncate(time.Second)

	good := watchSessionRow{UserID: userID, SessionID: "go-test-good", VideoID: videoID, TitleID: titleID,
		Client: "web", StartedAt: now, LastSeen: now, Seconds: 30}
	orphan := good
	orphan.SessionID, orphan.VideoID = "go-test-orphan", videoID+1_000_000_000
	if err := store.UpsertWatchSessions(context.Background(), []watchSessionRow{orphan, good}); err != nil {
		t.Fatalf("upsert with an orphan: %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM watch_sessions WHERE user_id = ? AND title_id = ?`,
		userID, titleID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows written = %d, want just the good one", n)
	}
}
