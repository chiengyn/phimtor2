package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The contributed-subtitle path writes to storage the ADMIN also owns, so the
// pieces that decide where bytes land, and the quota guard that decides how
// often, are worth pinning down. Everything here is DB-free.

func TestSubtitleStorageKey(t *testing.T) {
	titleID, episodeID := int64(12), int64(34)

	t.Run("groups by owner", func(t *testing.T) {
		got := subtitleStorageKey(&titleID, nil, "opensubtitles", "99", "vi", "vtt")
		if !strings.HasPrefix(got, "subtitles/title-12/") {
			t.Errorf("title key = %q", got)
		}
		got = subtitleStorageKey(nil, &episodeID, "subsource", "99:2", "vi", "vtt")
		if !strings.HasPrefix(got, "subtitles/episode-34/") {
			t.Errorf("episode key = %q", got)
		}
	})

	t.Run("hostile input cannot escape the store", func(t *testing.T) {
		// A provider file id is opaque and provider-defined (subsource packs
		// "id:episode" into it), so it reaches this unfiltered. Two defences
		// stack: the key builder strips anything outside [A-Za-z0-9._-], leaving
		// only the two structural slashes, and localBlobStore.path re-cleans.
		key := subtitleStorageKey(&titleID, nil, "sub source", "../../etc/passwd", "vi/../x", "vtt")
		if n := strings.Count(key, "/"); n != 2 {
			t.Errorf("injected path separators: %q has %d slashes", key, n)
		}
		base := t.TempDir()
		store, err := newLocalBlobStore(base)
		if err != nil {
			t.Fatalf("newLocalBlobStore: %v", err)
		}
		got := store.path(key)
		if !strings.HasPrefix(filepath.Clean(got), filepath.Clean(base)+string(filepath.Separator)) {
			t.Errorf("path %q escaped base %q", got, base)
		}
	})

	t.Run("re-saves never collide", func(t *testing.T) {
		// The nanosecond suffix is what stops a viewer save clobbering an admin
		// one in the shared store, so deleting one can never remove another.
		a := subtitleStorageKey(&titleID, nil, "opensubtitles", "99", "vi", "vtt")
		b := subtitleStorageKey(&titleID, nil, "opensubtitles", "99", "vi", "vtt")
		if a == b {
			t.Errorf("identical keys for two saves: %q", a)
		}
	})
}

func TestClampBytes(t *testing.T) {
	// subtitles.name is VARCHAR(512) and release names are attacker-influenced.
	if got := clampBytes("hello", 512); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := clampBytes(strings.Repeat("a", 600), 512); len(got) != 512 {
		t.Errorf("len = %d, want 512", len(got))
	}
	// Must not split a multi-byte rune: "é" is 2 bytes, so a cut at 5 has to
	// step back to 4 rather than emit half a rune.
	got := clampBytes("aaaaéé", 5)
	if !utf8.ValidString(got) {
		t.Errorf("clamp split a rune: %q", got)
	}
	if got != "aaaa" {
		t.Errorf("got %q, want %q", got, "aaaa")
	}
}

func TestRateLimiter(t *testing.T) {
	t.Run("caps per key", func(t *testing.T) {
		l := newRateLimiter(2, time.Hour)
		if !l.allow("u1") || !l.allow("u1") {
			t.Fatal("first two calls should pass")
		}
		if l.allow("u1") {
			t.Error("third call should be refused")
		}
		// A different user is unaffected — the quota guard is per account.
		if !l.allow("u2") {
			t.Error("other key should be independent")
		}
	})

	t.Run("refused calls are not recorded", func(t *testing.T) {
		// Otherwise hammering the endpoint would keep pushing the window out and
		// the limit would never reopen.
		l := newRateLimiter(1, 50*time.Millisecond)
		if !l.allow("u1") {
			t.Fatal("first call should pass")
		}
		for i := 0; i < 5; i++ {
			if l.allow("u1") {
				t.Fatal("should still be refused")
			}
		}
		time.Sleep(60 * time.Millisecond)
		if !l.allow("u1") {
			t.Error("window should have reopened")
		}
	})

	t.Run("nil limiter allows everything", func(t *testing.T) {
		// newRateLimiter(0, …) returns nil, which is how a cap is disabled.
		var l *rateLimiter
		if !l.allow("u1") {
			t.Error("nil limiter must not block")
		}
		if newRateLimiter(0, time.Hour) != nil {
			t.Error("zero limit should disable the cap")
		}
	})
}

func TestLocalBlobStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := newLocalBlobStore(filepath.Join(dir, "subtitles"))
	if err != nil {
		t.Fatalf("newLocalBlobStore: %v", err)
	}
	ctx := context.Background()
	titleID := int64(7)
	key := subtitleStorageKey(&titleID, nil, "opensubtitles", "1", "vi", "vtt")

	if err := store.Put(ctx, key, []byte("WEBVTT\n\n"), "text/vtt"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, key)
	if err != nil || string(got) != "WEBVTT\n\n" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	// Delete is used for exactly one thing: rolling back a blob whose row insert
	// failed. It must leave nothing behind, and be safe to call twice.
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, errBlobNotFound) {
		t.Errorf("Get after Delete = %v, want errBlobNotFound", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Errorf("Delete on missing key = %v, want nil", err)
	}
}

func TestSubtitleServiceNilSafe(t *testing.T) {
	// The whole rollback story is "unset the keys": the service goes nil, the
	// routes are not registered and the watch page renders no panel. Every
	// accessor has to survive that.
	var s *subtitleService
	if s.enabled() {
		t.Error("nil service must not report enabled")
	}
	if s.provider("") != nil || s.provider("opensubtitles") != nil {
		t.Error("nil service must yield no provider")
	}
	if len(s.enabledNames()) != 0 {
		t.Error("nil service must list no providers")
	}
	if newSubtitleService(Config{}) != nil {
		t.Error("no keys configured should yield a nil service")
	}
}

func TestSubtitleSearchEnabledRequiresAccounts(t *testing.T) {
	withKey := Config{OpenSubtitlesAPIKey: "k"}
	if withKey.subtitleSearchEnabled() {
		// Accounts off: the endpoints sit behind requireUser and the anonymous
		// panel's sign-in link would point at an unregistered route.
		t.Error("must stay off without accounts")
	}
	withAccounts := Config{
		OpenSubtitlesAPIKey: "k",
		GoogleClientID:      "id", GoogleClientSecret: "secret",
	}
	if !withAccounts.subtitleSearchEnabled() {
		t.Error("should be on with a key plus accounts")
	}
	noKeys := Config{GoogleClientID: "id", GoogleClientSecret: "secret"}
	if noKeys.subtitleSearchEnabled() {
		t.Error("accounts alone must not enable it")
	}
	subsourceOnly := Config{
		SubSourceAPIKey: "k",
		GoogleClientID:  "id", GoogleClientSecret: "secret",
	}
	if !subsourceOnly.subtitleSearchEnabled() {
		t.Error("either provider key should suffice")
	}
}
