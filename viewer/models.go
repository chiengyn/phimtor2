package main

import (
	"encoding/json"
	"strings"
	"time"
)

// Domain types loaded from the shared MySQL database (owned and populated by the
// admin service) and rendered by the HTML templates. Dates are kept as
// "YYYY-MM-DD" strings (empty when unknown), matching the MySQL DATE columns.

type Title struct {
	ID               int64
	TMDBID           int
	Type             string // "movie" | "tv"
	Title            string
	OriginalTitle    string
	Overview         string
	AirDate          string
	Runtime          *int
	PosterPath       string
	BackdropPath     string
	VoteAverage      float64
	OriginalLanguage string
	Status           string
	Genres           []Genre
	Seasons          []Season
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Genre struct {
	ID   int
	Name string
}

type Season struct {
	ID           int64
	SeasonNumber int
	Name         string
	Overview     string
	AirDate      string
	PosterPath   string
	Episodes     []Episode
}

type Episode struct {
	ID            int64
	EpisodeNumber int
	Name          string
	Overview      string
	AirDate       string
	Runtime       *int
	StillPath     string
}

// Video is one playable file for a movie or a single TV episode, at a given
// resolution. Exactly one of TitleID / EpisodeID is set. InfoHash / Magnet come
// from the joined torrent_sources row; the raw .torrent bytes (TorrentFile) are
// loaded only by GetVideo. Mirrors admin's Video (the viewer reads what admin
// writes); the viewer never sends Magnet to the browser — it POSTs it to the
// streamer server-side.
type Video struct {
	ID         int64  `json:"id"`
	SourceID   int64  `json:"source_id,omitempty"`
	TitleID    *int64 `json:"title_id,omitempty"`
	EpisodeID  *int64 `json:"episode_id,omitempty"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"` // "2160p" | "1080p" | "720p"
	InfoHash   string `json:"info_hash"`
	Magnet     string `json:"-"` // server-side only; never serialized to the browser
	// TorrentFile holds the raw .torrent bytes when the source has them stored
	// (magnet-only sources leave it nil). Server-side only — sent to the streamer
	// to skip the DHT metadata fetch, never serialized to the browser. Only the
	// single-video GetVideo load populates it; the list queries skip the blob.
	TorrentFile []byte    `json:"-"`
	FileIndex   int       `json:"file_index"`
	FilePath    string    `json:"file_path"`
	FileSize    int64     `json:"file_size"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

// Subtitle is one persisted subtitle file attached to a movie or single episode
// (exactly one of TitleID / EpisodeID set). The file lives in a BlobStore:
// StorageBackend names which one ("local" | "s3") and StorageKey is its key.
// Mirrors admin's Subtitle; the viewer reads it read-only.
type Subtitle struct {
	ID             int64           `json:"id"`
	TitleID        *int64          `json:"title_id,omitempty"`
	EpisodeID      *int64          `json:"episode_id,omitempty"`
	Provider       string          `json:"provider"`
	ProviderFileID string          `json:"provider_file_id"`
	Language       string          `json:"language"`
	Name           string          `json:"name"`
	DownloadCount  int             `json:"download_count"`
	Format         string          `json:"format"`
	StorageBackend string          `json:"storage_backend"`
	StorageKey     string          `json:"storage_key"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"created_at,omitempty"`
}

// Invoice is one crypto payment request. It is also the ledger: nothing else
// records that money arrived, so rows are never deleted, only moved between
// statuses.
//
// PayAmount / Received are decimal STRINGS, never float64. They are compared
// against a DECIMAL(36,18) column and the comparison must be exact — the amount
// is the invoice's identity (see billing.go), so a rounding error is not a small
// discrepancy, it is a payment credited to the wrong invoice or to none at all.
type Invoice struct {
	ID             int64
	Ref            string // opaque public id used in URLs — never expose ID
	UserID         int64
	Kind           string // "pass" | "title"
	PlanCode       string
	TitleID        *int64
	AmountUSDCents int
	Chain          string // the chain we SUGGESTED
	PayTo          string
	PayAmount      string
	Token          string
	Status         string // "pending" | "paid" | "expired"
	Received       string
	PaidChain      string // where it actually landed, may differ from Chain
	TxHash         string
	ExpiresAt      time.Time
	PaidAt         *time.Time
	CreatedAt      time.Time
}

// Paid reports whether this invoice has settled.
func (i *Invoice) Paid() bool { return i != nil && i.Status == "paid" }

// Pending reports whether this invoice is still awaiting payment.
func (i *Invoice) Pending() bool { return i != nil && i.Status == "pending" }

// User is a signed-in visitor. Sign-in is delegated to an OIDC provider (Google
// today), so no credential material lives here — the identity is the provider's
// stable subject id, never the email address, which may change.
//
// This is the ONE part of the catalog the viewer writes (see store.go): a row is
// created or refreshed on each login. Plan / PlanExpiresAt carry the paid 4K
// pass: a purchase pushes the expiry out (see the billing settle path) rather
// than writing a row to a subscriptions table, so the per-request entitlement
// check costs nothing beyond the user load the session middleware already does.
type User struct {
	ID        int64
	Email     string
	Name      string
	AvatarURL string
	Plan      string
	// PlanExpiresAt is when the time-based 4K pass runs out, nil for a user who
	// has never bought one. Always compare through HasPass — a non-nil expiry in
	// the past is an EXPIRED pass, not an active one.
	PlanExpiresAt *time.Time
	// CompExpiresAt is when an ADMIN-GRANTED 4K unlock runs out, nil when none
	// was granted. Written only by the admin service, which is why it is a
	// separate column from PlanExpiresAt: the two writers stay disjoint, so
	// revoking a comp cannot cancel a pass the user paid for. A permanent grant
	// is stored as the maximum DATETIME, so this side needs no notion of
	// "forever" — HasComp just compares against now, like HasPass.
	CompExpiresAt *time.Time

	// SavedIDs is the set of title ids this user has saved for later, loaded
	// alongside the user by the currentUser middleware so the header badge and
	// every card's save button render in their correct state on first paint —
	// no extra round trip, no flash of "unsaved".
	SavedIDs map[int64]bool
}

// SavedCount is how many titles the user has saved, for the header badge.
func (u *User) SavedCount() int { return len(u.SavedIDs) }

// HasPass reports whether the time-based 4K pass is active right now. It is
// nil-safe on both the receiver and the expiry so callers can ask it of an
// anonymous visitor without a guard.
func (u *User) HasPass(now time.Time) bool {
	return u != nil && u.PlanExpiresAt != nil && u.PlanExpiresAt.After(now)
}

// HasComp reports whether an admin-granted 4K unlock is active right now. Same
// nil-safety and same shape as HasPass — a permanent grant is just a very
// distant expiry, so there is no special case here.
func (u *User) HasComp(now time.Time) bool {
	return u != nil && u.CompExpiresAt != nil && u.CompExpiresAt.After(now)
}

// EntitledUntil is when this user's every-4K access ends: the later of the paid
// pass and the admin comp, ignoring whichever has already lapsed. nil when they
// hold neither. Used to tell a already-entitled visitor what they have instead
// of trying to sell it to them again.
func (u *User) EntitledUntil(now time.Time) *time.Time {
	var out *time.Time
	if u.HasPass(now) {
		out = u.PlanExpiresAt
	}
	if u.HasComp(now) && (out == nil || u.CompExpiresAt.After(*out)) {
		out = u.CompExpiresAt
	}
	return out
}

// DisplayName is what the header shows: the provider's name, falling back to the
// local part of the email so the chip is never blank.
func (u *User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return "Tài khoản"
}

// Initial is the single-letter avatar shown when the provider gave us no
// picture, or when the picture fails to load.
func (u *User) Initial() string {
	for _, r := range u.DisplayName() {
		return strings.ToUpper(string(r))
	}
	return "?"
}
