package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeWatchLog is a watchLog on a hand-cranked clock with a recording writer,
// so every test is hermetic: no database, no sleeping.
type fakeWatchLog struct {
	*watchLog
	clock  time.Time
	writes [][]watchSessionRow
	fail   bool
}

func newFakeWatchLog() *fakeWatchLog {
	f := &fakeWatchLog{clock: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	f.watchLog = newWatchLog(time.Minute, func(_ context.Context, rows []watchSessionRow) error {
		if f.fail {
			return errors.New("db down")
		}
		f.writes = append(f.writes, append([]watchSessionRow(nil), rows...))
		return nil
	})
	f.now = func() time.Time { return f.clock }
	return f
}

func (f *fakeWatchLog) advance(d time.Duration) { f.clock = f.clock.Add(d) }

// lastWrite returns the most recent flush, failing if there was none.
func (f *fakeWatchLog) lastWrite(t *testing.T) []watchSessionRow {
	t.Helper()
	if len(f.writes) == 0 {
		t.Fatal("nothing was written")
	}
	return f.writes[len(f.writes)-1]
}

func episodeVideo(id, episodeID int64, res string) *Video {
	return &Video{ID: id, EpisodeID: &episodeID, Resolution: res}
}

// A heartbeat for a torrent the user never prepared is not guessed at.
func TestWatchLogIgnoresUnpreparedBeats(t *testing.T) {
	f := newFakeWatchLog()
	f.beat(1, "tab", "hash")
	f.prepared(2, "hash", &Video{ID: 9}, 5, "web") // someone else's prepare
	f.beat(1, "tab", "hash")
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("wrote %v for a beat nobody prepared", f.writes)
	}
}

// Beats accumulate wall time, but a long silence (a sleeping laptop) counts
// only the gap cap, not the hours.
func TestWatchLogAccumulatesWithCappedGaps(t *testing.T) {
	f := newFakeWatchLog()
	f.prepared(1, "hash", episodeVideo(9, 77, "1080p"), 5, "tv")
	start := f.clock
	f.beat(1, "tab", "hash")
	for range 6 {
		f.advance(10 * time.Second)
		f.beat(1, "tab", "hash")
	}
	f.advance(3 * time.Hour)
	f.beat(1, "tab", "hash")
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := f.lastWrite(t)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if want := 60 + int(watchLogMaxBeatGap/time.Second); r.Seconds != want {
		t.Errorf("seconds = %d, want %d", r.Seconds, want)
	}
	if r.UserID != 1 || r.VideoID != 9 || r.TitleID != 5 || r.EpisodeID != 77 ||
		r.Resolution != "1080p" || r.Client != "tv" || !r.StartedAt.Equal(start) || !r.LastSeen.Equal(f.clock) {
		t.Errorf("row = %+v", r)
	}
}

// Switching source on the same page is a second session, not one session that
// silently changed video halfway.
func TestWatchLogSourceSwitchStartsANewRow(t *testing.T) {
	f := newFakeWatchLog()
	f.prepared(1, "h1080", &Video{ID: 9, Resolution: "1080p"}, 5, "web")
	f.beat(1, "tab", "h1080")
	f.advance(10 * time.Second)
	f.prepared(1, "h2160", &Video{ID: 10, Resolution: "2160p"}, 5, "web")
	f.beat(1, "tab", "h2160")
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows := f.lastWrite(t); len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
}

// A failed flush keeps the session and retries it later with the same
// cumulative totals (plus anything new), never a delta that could double-count.
func TestWatchLogRetriesFailedFlushWithCumulativeTotals(t *testing.T) {
	f := newFakeWatchLog()
	f.prepared(1, "hash", &Video{ID: 9}, 5, "web")
	f.beat(1, "tab", "hash")
	f.advance(10 * time.Second)
	f.beat(1, "tab", "hash")

	f.fail = true
	if err := f.flush(context.Background()); err == nil {
		t.Fatal("flush swallowed the write error")
	}
	f.fail = false
	f.advance(10 * time.Second)
	f.beat(1, "tab", "hash")
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.lastWrite(t)[0].Seconds; got != 20 {
		t.Errorf("seconds after retry = %d, want 20", got)
	}

	// Nothing changed since: the next flush has nothing to say.
	before := len(f.writes)
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != before {
		t.Error("a clean session was written again")
	}
}

// Finished sessions are forgotten once flushed; a session the database keeps
// refusing is eventually dropped so memory cannot grow without bound.
func TestWatchLogEvictsIdleSessions(t *testing.T) {
	f := newFakeWatchLog()
	f.prepared(1, "hash", &Video{ID: 9}, 5, "web")
	f.beat(1, "tab", "hash")
	if err := f.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.advance(watchLogIdleEvict + time.Second)
	_ = f.flush(context.Background())
	if n := len(f.sessions); n != 0 {
		t.Errorf("%d flushed idle sessions kept", n)
	}

	f.fail = true
	f.beat(1, "tab2", "hash")
	f.advance(watchLogIdleEvict + time.Second)
	_ = f.flush(context.Background())
	if n := len(f.sessions); n != 1 {
		t.Fatalf("an unflushed session was dropped early (%d left)", n)
	}
	f.advance(watchLogDirtyGiveUp)
	_ = f.flush(context.Background())
	if n := len(f.sessions); n != 0 {
		t.Errorf("an unflushable session was kept forever (%d left)", n)
	}

	f.advance(watchLogPrepareTTL)
	_ = f.flush(context.Background())
	if n := len(f.prepares); n != 0 {
		t.Errorf("%d stale prepares kept", n)
	}
}

// Shutdown (every deploy) flushes what is pending before run returns.
func TestWatchLogFlushesOnShutdown(t *testing.T) {
	f := newFakeWatchLog()
	f.interval = time.Hour // the ticker never fires during the test
	f.prepared(1, "hash", &Video{ID: 9}, 5, "web")
	f.beat(1, "tab", "hash")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if len(f.writes) != 1 {
		t.Fatalf("final flush wrote %d times, want 1", len(f.writes))
	}
}

// Disabled (nil) is a no-op everywhere, so call sites need no guards.
func TestWatchLogNilIsANoOp(t *testing.T) {
	var l *watchLog
	l.prepared(1, "hash", &Video{ID: 9}, 5, "web")
	l.beat(1, "tab", "hash")
	if err := l.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.run(context.Background())
}
