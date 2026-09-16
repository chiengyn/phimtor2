package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/text/language"
)

type Locale string

const (
	LocaleVI Locale = "vi"
	LocaleEN Locale = "en"
)

var supportedLocales = []Locale{LocaleVI, LocaleEN}

type localeContextKey struct{}

func parseLocale(raw string) (Locale, bool) {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "_", "-")
	switch {
	case normalized == "vi" || strings.HasPrefix(normalized, "vi-"):
		return LocaleVI, true
	case normalized == "en" || strings.HasPrefix(normalized, "en-"):
		return LocaleEN, true
	default:
		return "", false
	}
}

func localeFromContext(ctx context.Context) Locale {
	if locale, ok := ctx.Value(localeContextKey{}).(Locale); ok {
		return locale
	}
	return LocaleVI
}

func fallbackLocale(locale Locale) Locale {
	if locale == LocaleEN {
		return LocaleVI
	}
	return LocaleEN
}

func localeOrigin(locale Locale) string { return "/" + string(locale) }

func localeHome(locale Locale) string { return localeOrigin(locale) + "/" }

func localeFromRequestPath(path string) Locale {
	for _, locale := range supportedLocales {
		prefix := localeOrigin(locale)
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return locale
		}
	}
	return LocaleVI
}

func alternateLocaleURL(r *http.Request) string {
	current := localeFromContext(r.Context())
	target := fallbackLocale(current)
	path := r.URL.Path
	prefix := localeOrigin(current)
	if path == prefix {
		path = localeOrigin(target)
	} else if strings.HasPrefix(path, prefix+"/") {
		path = localeOrigin(target) + strings.TrimPrefix(path, prefix)
	} else {
		path = localeHome(target)
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	return path
}

func preferredLocale(r *http.Request) Locale {
	if cookie, err := r.Cookie("phimnet_locale"); err == nil {
		if locale, ok := parseLocale(cookie.Value); ok {
			return locale
		}
	}
	tags, _, err := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	if err == nil {
		for _, tag := range tags {
			base, _ := tag.Base()
			locale, ok := parseLocale(base.String())
			if ok {
				return locale
			}
		}
	}
	// Be forgiving of a malformed header: browsers normally emit valid syntax,
	// but a simple first recognizable language still gives a sensible result.
	for _, item := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		if locale, ok := parseLocale(strings.TrimSpace(strings.SplitN(item, ";", 2)[0])); ok {
			return locale
		}
	}
	return LocaleVI
}

func setLocaleCookie(w http.ResponseWriter, r *http.Request, locale Locale) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: "phimnet_locale", Value: string(locale), Path: "/",
		MaxAge: int((365 * 24 * time.Hour).Seconds()), SameSite: http.SameSiteLaxMode,
		Secure: secure, HttpOnly: true,
	})
}

func localizeLegacyURL(locale Locale, r *http.Request) string {
	path := localeOrigin(locale) + r.URL.Path
	if r.URL.Path == "/" {
		path = localeHome(locale)
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	return path
}

func localeURL(locale Locale, path string) string {
	if path == "" || path == "/" {
		return localeHome(locale)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return localeOrigin(locale) + path
}

func localeQueryURL(locale Locale, path string, values url.Values) string {
	out := localeURL(locale, path)
	if encoded := values.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out
}

func formatDate(locale Locale, value time.Time) string {
	if locale == LocaleEN {
		return value.Format("Jan 2, 2006")
	}
	return value.Format("02/01/2006")
}

var messages = map[Locale]map[string]string{
	LocaleVI: {
		"language.name":        "English",
		"language.switch_aria": "Chuyển sang tiếng Anh",
		"nav.home":             "Trang chủ", "nav.movies": "Phim lẻ", "nav.tv": "Phim bộ", "nav.saved": "Đã lưu",
		"account.label": "Tài khoản", "account.sign_in": "Đăng nhập", "account.sign_in_google": "Đăng nhập với Google",
		"account.logout": "Đăng xuất", "account.upgrade": "Nâng cấp 4K",
		"discord.aria":   "Tham gia Discord hỗ trợ",
		"footer.tagline": "Xem phim lẻ & phim bộ online, phụ đề tiếng Việt.",
		"footer.support": "Cần hỗ trợ?", "footer.join_discord": "Tham gia kênh Discord của chúng tôi",
		"type.movie": "Phim lẻ", "type.tv": "Phim bộ", "unit.minutes": "phút",
		"home.title":          "phimnet – Xem phim lẻ, phim bộ online phụ đề tiếng Việt",
		"home.description":    "phimnet – Xem phim lẻ và phim bộ online miễn phí, phụ đề tiếng Việt, cập nhật phim mới mỗi ngày.",
		"home.og_title":       "phimnet – Xem phim online phụ đề tiếng Việt",
		"home.og_description": "Xem phim lẻ và phim bộ online miễn phí, phụ đề tiếng Việt, cập nhật mỗi ngày.",
		"home.featured":       "nổi bật", "home.featured_aria": "Phim nổi bật", "home.choose_featured": "Chọn phim nổi bật",
		"home.watch_now": "Xem ngay", "home.play": "Phát", "home.info": "Thông tin",
		"search.placeholder": "Tìm phim, chương trình...", "search.aria": "Tìm phim", "search.genres": "Thể loại",
		"search.all_genres": "Tất cả thể loại", "search.type": "Loại phim", "search.all": "Tất cả", "search.submit": "Tìm",
		"row.top10": "Top 10 nổi bật hôm nay", "row.subtitled": "Phim lẻ Vietsub mới cập nhật",
		"row.movies": "Phim lẻ", "row.tv": "Phim bộ", "row.prev": "Cuộn về trước", "row.next": "Cuộn tiếp", "row.rank": "Hạng %d: %s",
		"card.subtitle": "Vietsub", "card.empty": "Không tìm thấy phim nào.", "card.none": "Chưa có phim nào.",
		"bookmark.save": "Lưu xem sau", "bookmark.saved": "Đã lưu", "bookmark.remove": "Bỏ lưu",
		"pager.aria": "Phân trang", "pager.prev": "Trước", "pager.next": "Sau",
		"detail.title_suffix": "Xem online phụ đề tiếng Việt", "detail.fallback_description": "Xem %s %s online, phụ đề tiếng Việt, chất lượng cao tại phimnet.",
		"detail.watch": "Xem phim", "detail.seasons": "Các phần", "detail.season": "Phần %d", "detail.episode": "Tập %d", "detail.episode_count": "%d tập", "detail.watch_short": "Xem",
		"bookmarks.title": "Phim đã lưu", "bookmarks.description": "Danh sách phim bạn đã lưu để xem sau trên phimnet.",
		"bookmarks.clear": "Xoá tất cả", "bookmarks.empty": "Bạn chưa lưu phim nào. Bấm vào biểu tượng dấu trang trên áp phích phim để lưu xem sau.",
		"bookmarks.local_note":    "Danh sách này chỉ lưu trên trình duyệt hiện tại. Đăng nhập để đồng bộ trên mọi thiết bị.",
		"bookmarks.clear_confirm": "Xoá toàn bộ danh sách phim đã lưu?",
		"not_found.title":         "Không tìm thấy", "not_found.message": "Không tìm thấy phim bạn yêu cầu.", "not_found.home": "Về trang chủ",
		"plans.title": "Nâng cấp 4K", "plans.description": "Mở khoá chất lượng 4K trên phimnet.", "plans.heading": "Nâng cấp để xem 4K",
		"plans.active_forever": "Bạn đang có quyền xem 4K vĩnh viễn.", "plans.active_until": "Bạn đang có quyền xem 4K, hiệu lực đến %s. Mua thêm sẽ cộng dồn thời hạn.",
		"plans.sign_in_note": "Cần đăng nhập trước khi thanh toán, vì quyền xem 4K gắn với tài khoản của bạn.",
		"plans.pass30":       "Gói 30 ngày", "plans.pass365": "Gói 1 năm", "plans.title_unlock": "Mở khoá vĩnh viễn: %s",
		"plans.pass_note": "Mở khoá mọi phim 4K trong thời hạn của gói.", "plans.title_note": "Xem 4K phim này mãi mãi, không giới hạn thời gian.",
		"plans.owned": "Đã sở hữu", "plans.choose": "Chọn gói này", "plans.none": "Hiện chưa có gói nào khả dụng.",
		"plans.crypto": "Thanh toán bằng tiền mã hoá", "plans.chain_note": "Chọn mạng bạn muốn chuyển. Phí mạng do bạn trả, nên các mạng L2 thường rẻ hơn nhiều.",
		"plans.choose_chain": "Hãy chọn một mạng thanh toán.", "plans.creating": "Đang tạo hoá đơn…", "plans.create_error": "Không tạo được hoá đơn",
		"invoice.back": "Quay lại các gói", "invoice.paid": "Đã nhận thanh toán. Quyền xem 4K đã được kích hoạt cho tài khoản của bạn.",
		"invoice.start": "Bắt đầu xem", "invoice.expired": "Hoá đơn này đã hết hạn. Hãy tạo hoá đơn mới — số tiền cũ không còn được ghi nhận.",
		"invoice.new": "Tạo hoá đơn mới", "invoice.instructions": "Chuyển chính xác số tiền bên dưới. Số tiền là thứ nhận diện hoá đơn của bạn, nên chuyển sai số sẽ không được cộng tự động.",
		"invoice.qr_alt": "Mã QR địa chỉ nhận", "invoice.amount": "Số tiền chính xác", "invoice.address": "Địa chỉ nhận", "invoice.network": "Mạng",
		"invoice.copy": "Sao chép", "invoice.copied": "Đã sao chép", "invoice.also_accept": "Cũng chấp nhận trên:",
		"invoice.same_address": "cùng một địa chỉ",
		"invoice.valid_until":  "Hoá đơn có hiệu lực đến %s. Trang này tự cập nhật khi giao dịch được xác nhận.", "invoice.waiting": "Đang chờ thanh toán…",
		"watch.action": "Xem", "watch.description": "Xem %s online tại phimnet.", "watch.back": "Quay lại",
		"watch.notice":  "Phim đang được chuẩn bị — quá trình ban đầu có thể mất thời gian lâu hơn. Vui lòng chờ, video sẽ tự động phát khi sẵn sàng.",
		"watch.sources": "Nguồn phát", "watch.shortcuts": "Phím tắt", "watch.play_pause": "Phát / Dừng", "watch.seek": "Tua 10 giây",
		"watch.volume": "Âm lượng", "watch.mute": "Tắt tiếng", "watch.fullscreen": "Toàn màn hình", "watch.subtitles": "Phụ đề",
		"watch.upload_subtitle": "Tải tệp .srt/.vtt", "watch.customize_subtitle": "Tùy chỉnh hiển thị", "watch.subtitle_off": "Tắt phụ đề",
		"watch.font_size": "Cỡ chữ", "watch.text_color": "Màu chữ", "watch.background_color": "Màu nền", "watch.background_opacity": "Độ mờ nền",
		"watch.text_edge": "Viền chữ", "watch.edge_none": "Không", "watch.edge_shadow": "Bóng đổ", "watch.edge_outline": "Viền đen", "watch.reset": "Đặt lại mặc định",
		"watch.preparing": "Đang chuẩn bị…", "watch.no_source": "Chưa có nguồn phát cho nội dung này.",
		"watch.sign_in_quality": "Đăng nhập để xem chất lượng %s", "watch.sign_in": "Đăng nhập", "watch.upgrade_quality": "Nâng cấp để xem chất lượng %s",
		"watch.upgrade": "Nâng cấp", "watch.coming_soon_title": "Sắp ra mắt cho người dùng trả phí", "watch.coming_soon": "Sắp ra mắt",
		"watch.free_hint":   "Tài khoản miễn phí. Chất lượng 720p xem được ngay không cần đăng nhập.",
		"watch.crypto_hint": "Thanh toán bằng tiền mã hoá. Các chất lượng thấp hơn vẫn xem được bình thường.", "watch.view_plans": "Xem các gói",
		"watch.preparing_source": "Đang chuẩn bị nguồn phát…", "watch.prepare_error": "Không thể chuẩn bị nguồn phát: %s", "watch.loading_info": "Đang tải thông tin…",
		"watch.loading_info_seconds": "Đang tải thông tin… (%ds)", "watch.buffering": "Đang tải bộ đệm đầu phim… %d%%",
		"watch.slow":          "Nguồn phát đang tải chậm hoặc không có seeder. Vui lòng thử lại hoặc chọn nguồn khác.",
		"watch.compatibility": "Định dạng chưa được hỗ trợ trực tiếp, đang chuyển sang chế độ tương thích…",
		"watch.reconnecting":  "Mất kết nối nguồn phát, đang kết nối lại…", "watch.downloading": "Đang tải dữ liệu… nguồn có thể đang thiếu seeder.",
		"watch.metadata": "Đang tải metadata…", "watch.debug_shown": "Đã hiện bảng gỡ lỗi tải xuống.", "watch.debug_hidden": "Đã ẩn bảng gỡ lỗi.",
		"watch.unsupported_subtitle": "định dạng không hỗ trợ (dùng .srt hoặc .vtt)", "watch.subtitle_error": "Lỗi phụ đề: %s",
		"watch.subtitle_enabled": "Đang bật: %s", "watch.no_saved_subtitle": "Chưa có phụ đề đã lưu cho nội dung này.", "watch.subtitle_name": "Phụ đề",
		"watch.subtitle_load_error": "Không tải được phụ đề đã lưu: %s", "watch.unavailable_4k": "Các nguồn 4K hiện chưa khả dụng.",
		"watch.analyzing": "Đang phân tích định dạng video…", "watch.seeking": "Đang tua…",
		"watch.stat_download": "Tốc độ tải xuống", "watch.stat_upload": "Tốc độ tải lên", "watch.stat_seeders": "Seeder đang kết nối",
		"watch.stat_peers": "Peer hoạt động / tổng", "watch.stat_pending": "Peer đang chờ", "watch.stat_half_open": "Kết nối nửa mở", "watch.stat_pieces": "Mảnh đã hoàn thành",
		"watch.episode_sub": "Phần %d · Tập %d", "watch.source_unavailable": "nguồn này hiện không khả dụng",
		"billing.invalid_request": "yêu cầu không hợp lệ", "billing.invalid_plan": "gói không tồn tại", "billing.invalid_chain": "mạng thanh toán không hợp lệ",
		"billing.missing_title": "thiếu phim cần mở khoá", "billing.title_not_found": "không tìm thấy phim", "billing.already_unlocked": "bạn đã mở khoá phim này rồi",
	},
	LocaleEN: {
		"language.name":        "Tiếng Việt",
		"language.switch_aria": "Switch to Vietnamese",
		"nav.home":             "Home", "nav.movies": "Movies", "nav.tv": "TV Shows", "nav.saved": "Saved",
		"account.label": "Account", "account.sign_in": "Sign in", "account.sign_in_google": "Sign in with Google",
		"account.logout": "Sign out", "account.upgrade": "Upgrade to 4K",
		"discord.aria":   "Join the support Discord",
		"footer.tagline": "Watch movies and TV shows online with subtitles.",
		"footer.support": "Need help?", "footer.join_discord": "Join our Discord community",
		"type.movie": "Movie", "type.tv": "TV Show", "unit.minutes": "minutes",
		"home.title":          "phimnet – Watch movies and TV shows online",
		"home.description":    "Watch movies and TV shows online with subtitles, with new titles added every day.",
		"home.og_title":       "phimnet – Watch movies online",
		"home.og_description": "Watch movies and TV shows online with subtitles, updated daily.",
		"home.featured":       "featured", "home.featured_aria": "Featured titles", "home.choose_featured": "Choose a featured title",
		"home.watch_now": "Watch now", "home.play": "Play", "home.info": "More info",
		"search.placeholder": "Search movies and shows...", "search.aria": "Search titles", "search.genres": "Genre",
		"search.all_genres": "All genres", "search.type": "Content type", "search.all": "All", "search.submit": "Search",
		"row.top10": "Today's Top 10", "row.subtitled": "Recently updated movies with English subtitles",
		"row.movies": "Movies", "row.tv": "TV Shows", "row.prev": "Scroll backward", "row.next": "Scroll forward", "row.rank": "Rank %d: %s",
		"card.subtitle": "English CC", "card.empty": "No titles found.", "card.none": "No titles are available yet.",
		"bookmark.save": "Save for later", "bookmark.saved": "Saved", "bookmark.remove": "Remove from saved",
		"pager.aria": "Pagination", "pager.prev": "Previous", "pager.next": "Next",
		"detail.title_suffix": "Watch online", "detail.fallback_description": "Watch %s %s online with subtitles in high quality on phimnet.",
		"detail.watch": "Watch movie", "detail.seasons": "Seasons", "detail.season": "Season %d", "detail.episode": "Episode %d", "detail.episode_count": "%d episodes", "detail.watch_short": "Watch",
		"bookmarks.title": "Saved titles", "bookmarks.description": "Movies and shows you saved to watch later on phimnet.",
		"bookmarks.clear": "Clear all", "bookmarks.empty": "You have not saved anything yet. Use the bookmark icon on a poster to save it for later.",
		"bookmarks.local_note":    "This list is stored only in this browser. Sign in to sync it across devices.",
		"bookmarks.clear_confirm": "Clear your entire saved list?",
		"not_found.title":         "Not found", "not_found.message": "We could not find the title you requested.", "not_found.home": "Back to home",
		"plans.title": "Upgrade to 4K", "plans.description": "Unlock 4K streaming on phimnet.", "plans.heading": "Upgrade to watch in 4K",
		"plans.active_forever": "You have permanent access to 4K.", "plans.active_until": "You have 4K access until %s. Another purchase will extend it.",
		"plans.sign_in_note": "Sign in before paying because 4K access is attached to your account.",
		"plans.pass30":       "30-day pass", "plans.pass365": "1-year pass", "plans.title_unlock": "Permanent unlock: %s",
		"plans.pass_note": "Unlock every 4K title for the duration of the pass.", "plans.title_note": "Watch this title in 4K permanently, with no time limit.",
		"plans.owned": "Owned", "plans.choose": "Choose this plan", "plans.none": "No plans are currently available.",
		"plans.crypto": "Pay with cryptocurrency", "plans.chain_note": "Choose the network you want to use. You pay the network fee, so L2 networks are usually much cheaper.",
		"plans.choose_chain": "Choose a payment network.", "plans.creating": "Creating invoice…", "plans.create_error": "Could not create the invoice",
		"invoice.back": "Back to plans", "invoice.paid": "Payment received. 4K access is now active on your account.",
		"invoice.start": "Start watching", "invoice.expired": "This invoice has expired. Create a new one—the old amount will no longer be credited automatically.",
		"invoice.new": "Create a new invoice", "invoice.instructions": "Transfer the exact amount below. The amount identifies your invoice, so an incorrect amount cannot be credited automatically.",
		"invoice.qr_alt": "Receiving address QR code", "invoice.amount": "Exact amount", "invoice.address": "Receiving address", "invoice.network": "Network",
		"invoice.copy": "Copy", "invoice.copied": "Copied", "invoice.also_accept": "Also accepted on:",
		"invoice.same_address": "the same address",
		"invoice.valid_until":  "This invoice is valid until %s. This page updates automatically after confirmation.", "invoice.waiting": "Waiting for payment…",
		"watch.action": "Watch", "watch.description": "Watch %s online on phimnet.", "watch.back": "Back",
		"watch.notice":  "This title is being prepared. The initial load may take a little longer; playback will start automatically when it is ready.",
		"watch.sources": "Sources", "watch.shortcuts": "Keyboard shortcuts", "watch.play_pause": "Play / Pause", "watch.seek": "Seek 10 seconds",
		"watch.volume": "Volume", "watch.mute": "Mute", "watch.fullscreen": "Fullscreen", "watch.subtitles": "Subtitles",
		"watch.upload_subtitle": "Load .srt/.vtt file", "watch.customize_subtitle": "Customize appearance", "watch.subtitle_off": "Turn subtitles off",
		"watch.font_size": "Font size", "watch.text_color": "Text color", "watch.background_color": "Background color", "watch.background_opacity": "Background opacity",
		"watch.text_edge": "Text edge", "watch.edge_none": "None", "watch.edge_shadow": "Drop shadow", "watch.edge_outline": "Black outline", "watch.reset": "Reset to defaults",
		"watch.preparing": "Preparing…", "watch.no_source": "No source is available for this title.",
		"watch.sign_in_quality": "Sign in to watch in %s", "watch.sign_in": "Sign in", "watch.upgrade_quality": "Upgrade to watch in %s",
		"watch.upgrade": "Upgrade", "watch.coming_soon_title": "Coming soon for paid members", "watch.coming_soon": "Coming soon",
		"watch.free_hint":   "Accounts are free. You can watch 720p immediately without signing in.",
		"watch.crypto_hint": "Pay with cryptocurrency. Lower qualities remain available as usual.", "watch.view_plans": "View plans",
		"watch.preparing_source": "Preparing source…", "watch.prepare_error": "Could not prepare the source: %s", "watch.loading_info": "Loading information…",
		"watch.loading_info_seconds": "Loading information… (%ds)", "watch.buffering": "Buffering the beginning… %d%%",
		"watch.slow":          "This source is loading slowly or has no seeders. Try again or choose another source.",
		"watch.compatibility": "This format is not directly supported. Switching to compatibility mode…",
		"watch.reconnecting":  "Lost the source connection; reconnecting…", "watch.downloading": "Loading data… this source may be short on seeders.",
		"watch.metadata": "Loading metadata…", "watch.debug_shown": "Download debug panel shown.", "watch.debug_hidden": "Download debug panel hidden.",
		"watch.unsupported_subtitle": "unsupported format (use .srt or .vtt)", "watch.subtitle_error": "Subtitle error: %s",
		"watch.subtitle_enabled": "Enabled: %s", "watch.no_saved_subtitle": "No saved subtitles are available for this title.", "watch.subtitle_name": "Subtitle",
		"watch.subtitle_load_error": "Could not load the saved subtitle: %s", "watch.unavailable_4k": "4K sources are not currently available.",
		"watch.analyzing": "Analyzing video format…", "watch.seeking": "Seeking…",
		"watch.stat_download": "Download speed", "watch.stat_upload": "Upload speed", "watch.stat_seeders": "Connected seeders",
		"watch.stat_peers": "Active / total peers", "watch.stat_pending": "Pending peers", "watch.stat_half_open": "Half-open connections", "watch.stat_pieces": "Completed pieces",
		"watch.episode_sub": "Season %d · Episode %d", "watch.source_unavailable": "this source is currently unavailable",
		"billing.invalid_request": "invalid request", "billing.invalid_plan": "plan does not exist", "billing.invalid_chain": "invalid payment network",
		"billing.missing_title": "missing title to unlock", "billing.title_not_found": "title not found", "billing.already_unlocked": "you already unlocked this title",
	},
}

func tr(locale Locale, key string, args ...any) string {
	value := messages[locale][key]
	if value == "" {
		value = messages[LocaleVI][key]
	}
	if value == "" {
		value = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(value, args...)
	}
	return value
}

func validateMessages() error {
	for key := range messages[LocaleVI] {
		if _, ok := messages[LocaleEN][key]; !ok {
			return fmt.Errorf("English translation missing key %q", key)
		}
	}
	for key := range messages[LocaleEN] {
		if _, ok := messages[LocaleVI][key]; !ok {
			return fmt.Errorf("Vietnamese translation missing key %q", key)
		}
	}
	return nil
}
