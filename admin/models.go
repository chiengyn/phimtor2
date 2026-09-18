package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// Domain types shared by the TMDB client (which builds them), the store (which
// persists/loads them) and the HTTP layer (which serializes them). Dates are
// kept as "YYYY-MM-DD" strings (empty when unknown) so they map cleanly to both
// the MySQL DATE columns and JSON.

type Title struct {
	ID               int64              `json:"id"`
	TMDBID           int                `json:"tmdb_id"`
	Type             string             `json:"type"` // "movie" | "tv"
	Title            string             `json:"title"`
	OriginalTitle    string             `json:"original_title"`
	Overview         string             `json:"overview"`
	AirDate          string             `json:"air_date"`
	Runtime          *int               `json:"runtime"`
	PosterPath       string             `json:"poster_path"`
	BackdropPath     string             `json:"backdrop_path"`
	VoteAverage      float64            `json:"vote_average"`
	OriginalLanguage string             `json:"original_language"`
	Status           string             `json:"status"`
	Genres           []Genre            `json:"genres,omitempty"`
	Seasons          []Season           `json:"seasons,omitempty"`
	Videos           []Video            `json:"videos,omitempty"`    // movie sources (TV uses Episode.Videos)
	Subtitles        []Subtitle         `json:"subtitles,omitempty"` // movie subtitles (TV uses Episode.Subtitles)
	CreatedAt        time.Time          `json:"created_at,omitempty"`
	UpdatedAt        time.Time          `json:"updated_at,omitempty"`
	Translations     []TitleTranslation `json:"translations,omitempty"`
}

type TitleTranslation struct {
	Locale       string `json:"locale"`
	Title        string `json:"title"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

type Genre struct {
	ID           int                `json:"id"`
	Name         string             `json:"name"`
	Translations []GenreTranslation `json:"translations,omitempty"`
}

type GenreTranslation struct {
	Locale string `json:"locale"`
	Name   string `json:"name"`
}

type Season struct {
	ID           int64               `json:"id"`
	SeasonNumber int                 `json:"season_number"`
	Name         string              `json:"name"`
	Overview     string              `json:"overview"`
	AirDate      string              `json:"air_date"`
	PosterPath   string              `json:"poster_path"`
	Episodes     []Episode           `json:"episodes,omitempty"`
	Translations []SeasonTranslation `json:"translations,omitempty"`
}

type SeasonTranslation struct {
	Locale     string `json:"locale"`
	Name       string `json:"name"`
	Overview   string `json:"overview"`
	PosterPath string `json:"poster_path"`
}

type Episode struct {
	ID            int64                `json:"id"`
	EpisodeNumber int                  `json:"episode_number"`
	Name          string               `json:"name"`
	Overview      string               `json:"overview"`
	AirDate       string               `json:"air_date"`
	Runtime       *int                 `json:"runtime"`
	StillPath     string               `json:"still_path"`
	Videos        []Video              `json:"videos,omitempty"`
	Subtitles     []Subtitle           `json:"subtitles,omitempty"`
	Translations  []EpisodeTranslation `json:"translations,omitempty"`
}

type EpisodeTranslation struct {
	Locale   string `json:"locale"`
	Name     string `json:"name"`
	Overview string `json:"overview"`
}

// Video is one playable file for a movie or a single TV episode, at a given
// resolution. Exactly one of TitleID / EpisodeID is set. A video points at a
// torrent_sources row (SourceID) plus the FileIndex of its file inside that
// torrent; many videos can share one source (a season pack maps each episode to
// a different file in the same .torrent). InfoHash / Magnet are loaded from the
// source via JOIN. The raw .torrent bytes live on the source, never on the
// video, and are never sent to the browser.
type Video struct {
	ID         int64  `json:"id"`
	SourceID   int64  `json:"source_id,omitempty"`
	TitleID    *int64 `json:"title_id,omitempty"`
	EpisodeID  *int64 `json:"episode_id,omitempty"`
	Name       string `json:"name"`
	Resolution string `json:"resolution"` // "2160p" | "1080p" | "720p"
	InfoHash   string `json:"info_hash"`
	Magnet     string `json:"magnet"`
	// HasTorrentFile is true when the source has the raw .torrent bytes stored
	// (torrent_file IS NOT NULL); magnet-only sources are false. The bytes
	// themselves are never sent to the browser, only this flag.
	HasTorrentFile bool      `json:"has_torrent_file"`
	FileIndex      int       `json:"file_index"`
	FilePath       string    `json:"file_path"`
	FileSize       int64     `json:"file_size"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// Subtitle is one persisted subtitle file attached to a movie or a single TV
// episode (exactly one of TitleID / EpisodeID is set, mirroring Video). It was
// fetched from Provider (e.g. "opensubtitles") — ProviderFileID, DownloadCount
// and any extra Metadata are kept so the same file can be re-identified — and
// the file itself lives in a BlobStore: StorageBackend names which one ("local"
// or "s3") and StorageKey is its key/path there. Format is the on-disk format
// (currently always "vtt").
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
	// AddedByUserID is the viewer account that contributed this subtitle, or nil
	// for the admin-curated rows this service writes. THIS SERVICE NEVER SETS IT
	// (see migrations/0011): it reads it only so contributed rows can be told
	// apart for moderation. The two writers are kept disjoint by row, not column.
	AddedByUserID *int64 `json:"added_by_user_id,omitempty"`
}

// User is a registered viewer account. Sign-in is delegated to an OIDC provider
// (Google today), so no credential material lives here — the identity is
// (Provider, ProviderUID), never the email address, which is descriptive only
// and may change. The admin mostly reports on these — "who signed up" — with
// one exception: it grants and revokes the complimentary 4K unlock.
//
// Plan / PlanExpiresAt are the PAID 4K pass and belong to the viewer, which
// writes them when an invoice settles. CompExpiresAt / CompGrantedAt are the
// admin's grant. They are separate columns so the two services never write the
// same data, which is what makes revoking a comp safe.
type User struct {
	ID            int64  `json:"id"`
	Provider      string `json:"provider"`
	ProviderUID   string `json:"provider_uid"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	AvatarURL     string `json:"avatar_url"`
	Plan          string `json:"plan"`
	// PlanExpiresAt is when the time-based 4K pass runs out, nil for an account
	// that never bought one. A non-nil value in the past is an expired pass.
	PlanExpiresAt *time.Time `json:"plan_expires_at,omitempty"`
	// CompExpiresAt / CompGrantedAt are the ADMIN-GRANTED 4K unlock — the only
	// columns on `users` this service writes. They are deliberately separate
	// from Plan/PlanExpiresAt, which belong to the viewer's billing path, so the
	// two writers never touch the same data and revoking a comp cannot cancel a
	// pass somebody paid for. A permanent grant is stored as the maximum
	// DATETIME (see migrations/0009_comp_premium.sql).
	CompExpiresAt *time.Time `json:"comp_expires_at,omitempty"`
	CompGrantedAt *time.Time `json:"comp_granted_at,omitempty"`
	IsBlocked     bool       `json:"is_blocked"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// BookmarkCount is how many titles this user has saved for later, loaded
	// alongside the row so the list shows engagement at a glance.
	BookmarkCount int `json:"bookmark_count"`
}

// compForeverDate is how a permanent grant is stored: the maximum DATETIME both
// MySQL 8 and MariaDB accept. Using an "end of time" date rather than a separate
// boolean keeps the entitlement check a single comparison against now, on both
// sides of the database — see migrations/0009_comp_premium.sql.
var compForeverDate = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// compUntil translates the admin form's duration choice into an expiry, or nil
// to revoke. The second return is false for an unrecognised choice: the column
// is a bare DATETIME with no constraint, so this is the only validation there is.
func compUntil(choice string) (*time.Time, bool) {
	switch choice {
	case "30d":
		t := time.Now().AddDate(0, 0, 30)
		return &t, true
	case "1y":
		t := time.Now().AddDate(1, 0, 0)
		return &t, true
	case "forever":
		t := compForeverDate
		return &t, true
	case "revoke":
		return nil, true
	}
	return nil, false
}

// HasPass reports whether a PAID 4K pass is active. These helpers take no
// argument because their only caller is the template — a rendered page has one
// notion of "now" and threading it through would buy nothing.
//
// It exists because `{{ with .PlanExpiresAt }}` was showing the 4K badge for
// accounts whose pass had already EXPIRED.
func (u User) HasPass() bool {
	return u.PlanExpiresAt != nil && u.PlanExpiresAt.After(time.Now())
}

// HasComp reports whether an admin-granted 4K unlock is active.
func (u User) HasComp() bool {
	return u.CompExpiresAt != nil && u.CompExpiresAt.After(time.Now())
}

// CompForever reports whether the active grant is the permanent one.
func (u User) CompForever() bool {
	return u.CompExpiresAt != nil && u.CompExpiresAt.Year() >= compForeverDate.Year()
}

// CompLabel describes the grant for the badge, never printing the year 9999.
func (u User) CompLabel() string {
	switch {
	case !u.HasComp():
		return ""
	case u.CompForever():
		return "vĩnh viễn"
	}
	return "đến " + u.CompExpiresAt.Format("02/01/2006")
}

// DisplayName is what the users list shows: the provider's name, falling back to
// the local part of the email so the column is never blank.
func (u User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	if i := strings.IndexByte(u.Email, '@'); i > 0 {
		return u.Email[:i]
	}
	return "(chưa đặt tên)"
}

// Initial is the single-letter avatar fallback for a user whose provider gave us
// no picture (or whose picture fails to load).
func (u User) Initial() string {
	for _, r := range u.DisplayName() {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// --- Billing, read-only -------------------------------------------------------
//
// The VIEWER owns these rows: it opens an invoice when a visitor picks a plan
// and settles it when that invoice's unique exact amount lands on-chain
// (migrations/0008_billing.sql). This service only ever READS them, for the
// /payments monitor.
//
// Keep it that way. A "mark as paid" button here would make the admin a second
// writer of the settle path — able to grant a paid entitlement no money paid
// for, and able to race the poller for the same row. When the intent is to give
// somebody 4K for free, that is what the comp is for (SetUserComp): it is
// recorded in different columns precisely so revenue stays distinguishable from
// gifts.

// Invoice is one payment attempt. Money amounts come in two shapes and neither
// is a float: AmountUSDCents is the price in integer cents, and PayAmount /
// Received are the on-chain token amounts as DECIMAL(36,18) rendered to strings
// (18 decimals does not fit a float64 without losing the dust that identifies
// the payment). Only ever display them — never parse and re-emit.
type Invoice struct {
	ID             int64
	Ref            string // opaque public id used in the viewer's URLs
	UserID         int64
	Kind           string // "pass" | "title"
	PlanCode       string
	TitleID        *int64
	AmountUSDCents int
	Chain          string // the chain the buyer was OFFERED
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
	// Joined in for display — the invoice row itself holds only ids.
	UserEmail string
	UserName  string
	TitleName string
}

func (i Invoice) Paid() bool    { return i.Status == "paid" }
func (i Invoice) Pending() bool { return i.Status == "pending" }

// Expired covers both an invoice the poller has already marked expired and one
// that is still 'pending' but out of time (the poller flips those on its next
// tick, so for up to one interval the row lags reality).
func (i Invoice) Expired() bool {
	return i.Status == "expired" || (i.Pending() && i.ExpiresAt.Before(time.Now()))
}

// StatusClass is the badge CSS class, and StatusLabel its Vietnamese text.
func (i Invoice) StatusClass() string {
	switch {
	case i.Paid():
		return "paid"
	case i.Expired():
		return "expired"
	}
	return "pending"
}

func (i Invoice) StatusLabel() string {
	switch {
	case i.Paid():
		return "Đã thanh toán"
	case i.Status == "expired":
		return "Hết hạn"
	case i.Expired():
		return "Hết hạn (chờ dọn)"
	}
	return "Đang chờ"
}

// USD renders the price. Integer cents, so no rounding happens here.
func (i Invoice) USD() string {
	return fmt.Sprintf("$%d.%02d", i.AmountUSDCents/100, i.AmountUSDCents%100)
}

// Amount is the exact token amount owed, with the trailing zeros DECIMAL(36,18)
// pads it with removed. The dust at the end is not noise — it is what identifies
// this invoice among every other one sharing the receive address — so it is
// deliberately shown in full rather than rounded for looks.
func (i Invoice) Amount() string { return trimDecimal(i.PayAmount) + " " + i.Token }

// Product describes what was bought. A title unlock names the film when the
// title still exists (the FK is ON DELETE SET NULL, so a purchase outlives the
// catalog row and must still render).
func (i Invoice) Product() string {
	if i.Kind == "title" {
		if i.TitleName != "" {
			return "Mở khoá vĩnh viễn: " + i.TitleName
		}
		return "Mở khoá vĩnh viễn (phim đã bị xoá)"
	}
	switch i.PlanCode {
	case "pass30":
		return "Gói 30 ngày"
	case "pass365":
		return "Gói 1 năm"
	}
	if i.PlanCode != "" {
		return i.PlanCode
	}
	return "Gói 4K"
}

// Buyer is who paid, falling back through name → email → user id so the column
// is never blank.
func (i Invoice) Buyer() string {
	if i.UserName != "" {
		return i.UserName
	}
	if i.UserEmail != "" {
		return i.UserEmail
	}
	return fmt.Sprintf("#%d", i.UserID)
}

// SettledChain is where the money actually arrived, which is the interesting
// one: an invoice quoted on Base may legitimately be paid on BSC, because every
// EVM chain shares one receive address and one amount-reservation namespace.
func (i Invoice) SettledChain() string {
	if i.PaidChain != "" {
		return chainLabel(i.PaidChain)
	}
	return chainLabel(i.Chain)
}

// QuotedChain is the chain the invoice was quoted on, labelled — shown next to
// SettledChain only when the two differ.
func (i Invoice) QuotedChain() string { return chainLabel(i.Chain) }

// ChainSwitched reports whether the buyer paid on a different chain than the one
// they were quoted. Harmless by design, but worth showing rather than hiding.
func (i Invoice) ChainSwitched() bool {
	return i.PaidChain != "" && i.PaidChain != i.Chain
}

// ShortTx is the transaction hash abbreviated for a table cell.
func (i Invoice) ShortTx() string { return shortHex(i.TxHash) }

// ExplorerURL links the settling transaction to a block explorer, empty when
// there is no hash yet or the chain is unknown to the table below. It is
// template.URL because the value is built here from a fixed map plus a hash the
// scanner produced, not from anything a visitor can set.
func (i Invoice) ExplorerURL() template.URL {
	if i.TxHash == "" {
		return ""
	}
	chain := i.PaidChain
	if chain == "" {
		chain = i.Chain
	}
	base, ok := chainExplorers[chain]
	if !ok {
		return ""
	}
	return template.URL(base + i.TxHash)
}

// ShortRef abbreviates the 32-char public invoice ref.
func (i Invoice) ShortRef() string {
	if len(i.Ref) <= 10 {
		return i.Ref
	}
	return i.Ref[:10] + "…"
}

// PaymentTotals is the summary strip at the top of the monitor: the ledger's
// own numbers plus what is actually live right now, which are different
// questions. Revenue is in integer cents.
type PaymentTotals struct {
	Invoices     int
	PaidCount    int
	PaidCents    int
	Cents30d     int
	PendingNow   int
	ExpiredCount int
	// ActivePasses / ActiveComps / TitleUnlocks are the entitlements in force,
	// counted from users and user_title_unlocks rather than the ledger — a pass
	// bought last year is revenue but not an active entitlement.
	ActivePasses int
	ActiveComps  int
	TitleUnlocks int
}

func (t PaymentTotals) PaidUSD() string { return usdFromCents(t.PaidCents) }
func (t PaymentTotals) USD30d() string  { return usdFromCents(t.Cents30d) }
func (t PaymentTotals) NoRevenue() bool { return t.PaidCount == 0 }
func (t PaymentTotals) Conversion() int {
	if t.Invoices == 0 {
		return 0
	}
	return t.PaidCount * 100 / t.Invoices
}

// ChainCursor is how far one chain's scan has got, straight out of
// billing_chain_cursors. It is the closest thing to a liveness signal for the
// viewer's poller that this database holds.
type ChainCursor struct {
	Chain     string
	Cursor    string
	UpdatedAt time.Time
}

func (c ChainCursor) Label() string { return chainLabel(c.Chain) }

// cursorStaleAfter is when a motionless cursor stops being normal. Every chain
// on offer produces blocks every few seconds and the poller's default interval
// is 30s, so ten minutes without movement means the poller is stopped, the RPC
// endpoint is failing, or the chain was removed from BILLING_EVM_CHAINS.
const cursorStaleAfter = 10 * time.Minute

func (c ChainCursor) Stale() bool { return time.Since(c.UpdatedAt) > cursorStaleAfter }

// Age is how long since the cursor last moved, in words.
func (c ChainCursor) Age() string { return shortDuration(time.Since(c.UpdatedAt)) }

// PayToUse is one receive address the ledger has actually quoted to buyers, with
// how many invoices carry it.
//
// This exists because of a specific production failure: Kamal's YAML 1.1 parser
// read the unquoted 0x… address as an integer, and four invoices went out asking
// buyers to pay a 48-digit decimal number that no wallet could send to. Nothing
// errored — the poller scanned happily against a garbage log filter. Surfacing
// the addresses the ledger is really using, and flagging any that is not a valid
// address, turns that whole class of silent misconfiguration into something
// visible at a glance.
type PayToUse struct {
	Address  string
	Invoices int
	LastUsed time.Time
}

// Valid reports whether this address is a payable EVM one: 0x plus 40 hex
// digits. Deliberately a small duplicate of the viewer's isEVMAddress, because
// the two services share no package and a monitor that imported the thing it
// monitors would be worse.
//
// It must NOT be relaxed into "anything without an 0x prefix is some other
// rail's format" — the failure this catches, a decimal number where the address
// should be, has no 0x prefix either. When a non-EVM rail (Tron's base58 T…)
// lands, teach this about that shape specifically.
func (p PayToUse) Valid() bool {
	if len(p.Address) != 42 || !strings.HasPrefix(p.Address, "0x") {
		return false
	}
	for _, r := range p.Address[2:] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// chainLabel turns a stored chain key into its display name. A key this map has
// never heard of is shown as-is rather than hidden, so adding a rail to the
// viewer without touching the admin degrades to "bsc" instead of a blank cell.
func chainLabel(chain string) string {
	if l, ok := chainLabels[chain]; ok {
		return l
	}
	if chain == "" {
		return "—"
	}
	return chain
}

var chainLabels = map[string]string{
	"ethereum": "Ethereum",
	"base":     "Base",
	"arbitrum": "Arbitrum One",
	"bsc":      "BNB Smart Chain",
	"polygon":  "Polygon",
	"tron":     "Tron",
}

// chainExplorers maps a chain key to its explorer's transaction URL prefix.
var chainExplorers = map[string]string{
	"ethereum": "https://etherscan.io/tx/",
	"base":     "https://basescan.org/tx/",
	"arbitrum": "https://arbiscan.io/tx/",
	"bsc":      "https://bscscan.com/tx/",
	"polygon":  "https://polygonscan.com/tx/",
	"tron":     "https://tronscan.org/#/transaction/",
}

func usdFromCents(cents int) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// trimDecimal drops the zero padding DECIMAL(36,18) returns ("5.000100000..." ->
// "5.0001"), by string surgery only. Parsing these into a float to tidy them up
// would corrupt the very digits that identify an invoice.
func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// shortHex abbreviates a long hash for a table cell, keeping both ends so it can
// still be eyeballed against an explorer.
func shortHex(s string) string {
	if len(s) <= 18 {
		return s
	}
	return s[:10] + "…" + s[len(s)-6:]
}

// shortDuration renders an age in Vietnamese, coarsely — this is for "is the
// poller alive", where minutes matter and seconds do not.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "vừa xong"
	case d < time.Hour:
		return fmt.Sprintf("%d phút trước", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d giờ trước", int(d.Hours()))
	}
	return fmt.Sprintf("%d ngày trước", int(d.Hours()/24))
}
