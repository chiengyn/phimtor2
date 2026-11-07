package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// watchLog records who watched what into watch_sessions
// (admin/migrations/0013_watch_sessions.sql), without touching the database on
// the request path.
//
// It is fed by two calls the viewer already makes for every playback, so no
// client — web page or TV APK, old or new — had to change:
//
//   - handlePrepareSource knows the user, the video and the title, and learns
//     the torrent's infohash from the manager. prepared() remembers that
//     (user, infohash) → video mapping.
//   - the 10-second heartbeat knows only (session id, infohash), but it runs
//     behind currentUser, so the user is known too. beat() resolves the video
//     through the prepared map and adds the elapsed time to that session.
//
// The heartbeat cannot carry the video itself: one torrent can be a whole
// season pack, so an infohash alone does not say which episode is playing.
//
// Sessions are aggregated here and flushed on an interval. Each entry holds
// CUMULATIVE totals and the write is an upsert keyed by the session, so a flush
// that fails is retried with the same numbers on the next tick and can never
// double-count. What is lost on a crash is at most one interval; a normal
// shutdown (every deploy) flushes first — see run and main.go.
//
// Like watchTracker, this is single-instance in-memory state.
type watchLog struct {
	mu       sync.Mutex
	prepares map[watchPrepareKey]watchPrepared
	sessions map[watchSessionKey]*watchSessionEntry

	interval time.Duration
	now      func() time.Time
	write    func(ctx context.Context, rows []watchSessionRow) error
}

// Tunables. maxBeatGap is three heartbeats: enough to absorb a missed beat or a
// slow network, small enough that a laptop asleep on the watch page does not
// count as hours of watching.
const (
	watchLogMaxBeatGap   = 30 * time.Second
	watchLogIdleEvict    = 10 * time.Minute // a flushed session this quiet is over
	watchLogPrepareTTL   = 12 * time.Hour   // longer than any film or binge sitting
	watchLogDirtyGiveUp  = time.Hour        // stop retrying a session the DB keeps refusing
	watchLogMaxSessionID = 64               // the column width
	watchLogFinalTimeout = 5 * time.Second
)

type watchPrepareKey struct {
	userID   int64
	infoHash string
}

// watchPrepared is what prepare learned about a video, waiting for heartbeats.
type watchPrepared struct {
	videoID    int64
	titleID    int64
	episodeID  int64 // 0 for a movie
	resolution string
	client     string
	at         time.Time
}

type watchSessionKey struct {
	userID    int64
	sessionID string
	videoID   int64
}

type watchSessionEntry struct {
	watchPrepared
	startedAt time.Time
	lastSeen  time.Time
	seconds   int
	dirty     bool
}

// watchSessionRow is one row to upsert, a snapshot taken under the lock.
type watchSessionRow struct {
	UserID     int64
	SessionID  string
	VideoID    int64
	TitleID    int64
	EpisodeID  int64 // 0 → NULL
	Resolution string
	Client     string
	StartedAt  time.Time
	LastSeen   time.Time
	Seconds    int
}

func newWatchLog(interval time.Duration, write func(context.Context, []watchSessionRow) error) *watchLog {
	return &watchLog{
		prepares: map[watchPrepareKey]watchPrepared{},
		sessions: map[watchSessionKey]*watchSessionEntry{},
		interval: interval,
		now:      time.Now,
		write:    write,
	}
}

// prepared remembers which video a user is about to stream under infoHash.
// Called after the manager accepted the torrent. Nil-safe, so a disabled log
// costs the caller nothing.
func (l *watchLog) prepared(userID int64, infoHash string, video *Video, titleID int64, client string) {
	if l == nil || userID == 0 || infoHash == "" || video == nil || titleID == 0 {
		return
	}
	p := watchPrepared{
		videoID:    video.ID,
		titleID:    titleID,
		resolution: video.Resolution,
		client:     client,
	}
	if video.EpisodeID != nil {
		p.episodeID = *video.EpisodeID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p.at = l.now()
	l.prepares[watchPrepareKey{userID, infoHash}] = p
}

// beat records one heartbeat from a signed-in user's watch session. A beat for
// a torrent this user never prepared (a stale tab from before a restart, a
// forged body) is ignored rather than guessed at.
func (l *watchLog) beat(userID int64, sessionID, infoHash string) {
	if l == nil || userID == 0 || sessionID == "" || len(sessionID) > watchLogMaxSessionID || infoHash == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.prepares[watchPrepareKey{userID, infoHash}]
	if !ok {
		return
	}
	now := l.now()
	key := watchSessionKey{userID, sessionID, p.videoID}
	e := l.sessions[key]
	if e == nil {
		l.sessions[key] = &watchSessionEntry{watchPrepared: p, startedAt: now, lastSeen: now, dirty: true}
		return
	}
	if gap := now.Sub(e.lastSeen); gap > 0 {
		e.seconds += int(min(gap, watchLogMaxBeatGap) / time.Second)
	}
	e.lastSeen = now
	e.dirty = true
}

// flush writes every session that changed since the last successful flush, then
// forgets what is finished. The write runs outside the lock so heartbeats never
// wait on MySQL.
func (l *watchLog) flush(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	var rows []watchSessionRow
	for k, e := range l.sessions {
		if e.dirty {
			rows = append(rows, watchSessionRow{
				UserID: k.userID, SessionID: k.sessionID, VideoID: k.videoID,
				TitleID: e.titleID, EpisodeID: e.episodeID,
				Resolution: e.resolution, Client: e.client,
				StartedAt: e.startedAt, LastSeen: e.lastSeen, Seconds: e.seconds,
			})
		}
	}
	l.mu.Unlock()

	var err error
	if len(rows) > 0 {
		err = l.write(ctx, rows)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if err == nil {
		// Clear only what was written AND has not moved since: a beat that
		// landed during the write re-dirtied its entry with a later lastSeen,
		// and must go out next time.
		for _, r := range rows {
			if e := l.sessions[watchSessionKey{r.UserID, r.SessionID, r.VideoID}]; e != nil && e.lastSeen.Equal(r.LastSeen) {
				e.dirty = false
			}
		}
	}
	for k, e := range l.sessions {
		idle := now.Sub(e.lastSeen)
		switch {
		case !e.dirty && idle > watchLogIdleEvict:
			delete(l.sessions, k)
		case e.dirty && idle > watchLogDirtyGiveUp:
			log.Printf("watchlog: giving up on unflushed session (user %d, video %d, %ds)", k.userID, k.videoID, e.seconds)
			delete(l.sessions, k)
		}
	}
	for k, p := range l.prepares {
		if now.Sub(p.at) > watchLogPrepareTTL {
			delete(l.prepares, k)
		}
	}
	return err
}

// run flushes on a ticker until ctx is cancelled, then flushes once more with a
// fresh context so a deploy's SIGTERM does not discard the last interval. It
// returns only after that final flush; main.go waits for it.
func (l *watchLog) run(ctx context.Context) {
	if l == nil {
		return
	}
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), watchLogFinalTimeout)
			if err := l.flush(final); err != nil {
				log.Printf("watchlog: final flush: %v", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := l.flush(ctx); err != nil {
				log.Printf("watchlog: flush: %v", err)
			}
		}
	}
}
