package main

import (
	"net/http"
	"strings"
)

// adScriptMaxBytes keeps a paste inside the ad_settings.script and ad_slots.code
// TEXT columns, which hold 65,535 bytes. A real ad tag is a few hundred.
const adScriptMaxBytes = 60000

// adSlotDef describes one banner slot on the public site. Key is the ad_slots
// row key and must match a key in the viewer's adSlotKeys (viewer/ads.go): the
// two lists are duplicated by convention, like the query layers. A key only one
// side knows is harmless, it just never shows.
type adSlotDef struct {
	Key   string
	Name  string
	Where string
	Size  string
	// Wire is a top-to-bottom sketch of the page for the admin UI, one block per
	// entry. "ad" marks where this slot sits.
	Wire []string
}

// adSlotDefs lists the slots in page order. Every one is centered, sits outside
// the player and every control, and is clipped to the screen width, so a 728px
// banner on a phone is cut off rather than scrolling the page sideways.
var adSlotDefs = []adSlotDef{
	{
		Key:   "home_top",
		Name:  "Trang chủ — dưới banner phim nổi bật",
		Where: "Ngay dưới carousel phim nổi bật, phía trên ô tìm kiếm. Cũng hiện trên trang kết quả lọc/tìm kiếm.",
		Size:  "máy tính 728×90 · điện thoại 320×50 hoặc 320×100",
		Wire:  []string{"hero", "ad", "search", "row", "row"},
	},
	{
		Key:   "home_feed",
		Name:  "Trang chủ — giữa các hàng phim",
		Where: "Sau hàng phim thứ 3 trên trang chủ. Trên trang kết quả lọc: giữa lưới phim và nút phân trang.",
		Size:  "máy tính 728×90 · điện thoại 300×250 hoặc 320×100",
		Wire:  []string{"hero", "search", "row", "row", "row", "ad", "row"},
	},
	{
		Key:   "detail",
		Name:  "Trang thông tin phim",
		Where: "Dưới phần thông tin và mô tả phim, phía trên danh sách mùa/tập.",
		Size:  "máy tính 728×90 · điện thoại 300×250 hoặc 320×100",
		Wire:  []string{"info", "ad", "seasons"},
	},
	{
		Key:   "watch",
		Name:  "Trang xem phim — dưới trình phát",
		Where: "Dưới trình phát và hàng chọn nguồn, phía trên phần phụ đề. Không bao giờ che trình phát.",
		Size:  "máy tính 728×90 · điện thoại 320×50 hoặc 300×250",
		Wire:  []string{"title", "player", "chips", "ad", "subs"},
	},
	{
		Key:   "footer",
		Name:  "Cuối trang (mọi trang)",
		Where: "Phía trên chân trang, trên mọi trang có quảng cáo.",
		Size:  "máy tính 728×90 · điện thoại 320×50",
		Wire:  []string{"content", "content", "ad", "footer"},
	},
}

// adMobileMaxWidth is the widest screen, in CSS pixels, that gets a slot's
// mobile code. Shown on the page only: the viewer's adMobileQuery decides, so
// keep the two in step. A 728px banner plus the page gutter needs about 800.
const adMobileMaxWidth = 800

// adSlotView is one slot as the form shows it.
type adSlotView struct {
	adSlotDef
	Enabled    bool
	Code       string
	MobileCode string
	Error      string
}

// adsPage is the view model for GET /ads.
type adsPage struct {
	AdSettings
	Slots          []adSlotView
	Saved          bool
	Error          string
	MaxBytes       int
	MobileMaxWidth int
}

// handleAdsPage shows the master switch, the global script and every slot.
func (s *Server) handleAdsPage(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetAdSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	saved, err := s.store.GetAdSlots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slots := make([]adSlotView, len(adSlotDefs))
	for i, def := range adSlotDefs {
		row := saved[def.Key]
		slots[i] = adSlotView{adSlotDef: def, Enabled: row.Enabled, Code: row.Code, MobileCode: row.MobileCode}
	}
	render(w, "ads.html", adsPage{
		AdSettings:     settings,
		Slots:          slots,
		Saved:          r.URL.Query().Get("saved") == "1",
		MaxBytes:       adScriptMaxBytes,
		MobileMaxWidth: adMobileMaxWidth,
	})
}

// handleSaveAds stores the switch, the script and every slot in one go.
//
// The code runs unescaped on every visitor's page, so this POST is as sensitive
// as the SQL console's and needs the same sameOriginPost guard: basic auth is
// ambient, and a cross-site form could otherwise plant a script on the public site.
func (s *Server) handleSaveAds(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	page := adsPage{MaxBytes: adScriptMaxBytes, MobileMaxWidth: adMobileMaxWidth}
	page.Enabled = r.FormValue("enabled") == "1"
	page.Script = cleanAdCode(r.FormValue("script"))

	failed := false
	anySlot := false
	slots := make([]AdSlot, len(adSlotDefs))
	page.Slots = make([]adSlotView, len(adSlotDefs))
	for i, def := range adSlotDefs {
		v := adSlotView{
			adSlotDef:  def,
			Enabled:    r.FormValue("slot_"+def.Key+"_enabled") == "1",
			Code:       cleanAdCode(r.FormValue("slot_" + def.Key + "_code")),
			MobileCode: cleanAdCode(r.FormValue("slot_" + def.Key + "_mobile_code")),
		}
		if msg := validateAdCode(v.Code); msg != "" {
			v.Error = "Mã máy tính: " + msg
		} else if msg := validateAdCode(v.MobileCode); msg != "" {
			v.Error = "Mã điện thoại: " + msg
		} else if v.Enabled && v.Code == "" && v.MobileCode == "" {
			v.Error = "Không thể bật vị trí này khi chưa có đoạn mã."
		}
		failed = failed || v.Error != ""
		anySlot = anySlot || v.Enabled
		page.Slots[i] = v
		slots[i] = AdSlot{Key: def.Key, Enabled: v.Enabled, Code: v.Code, MobileCode: v.MobileCode}
	}
	if msg := validateAdCode(page.Script); msg != "" {
		page.Error = "Đoạn mã chung: " + msg
	} else if page.Enabled && page.Script == "" && !anySlot {
		page.Error = "Không thể bật quảng cáo khi chưa có đoạn mã chung hoặc vị trí nào được bật."
	} else if failed {
		page.Error = "Một số vị trí chưa hợp lệ, xem bên dưới."
	}

	if page.Error != "" {
		// Re-render what was typed, so a rejected paste is not lost.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		render(w, "ads.html", page)
		return
	}
	if err := s.store.SaveAds(r.Context(), page.Enabled, page.Script, slots); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ads?saved=1", http.StatusSeeOther)
}

// cleanAdCode normalizes a textarea: browsers submit CRLF line endings.
func cleanAdCode(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
}

// validateAdCode returns why a code field must be refused, or "" when it is
// fine. It catches paste mistakes, not malice: whoever passes basic auth is
// trusted with this field. The "<" check matters because JavaScript pasted
// without its <script> tags would print as visible text on the public site.
func validateAdCode(code string) string {
	switch {
	case len(code) > adScriptMaxBytes:
		return "Đoạn mã quá dài."
	case code != "" && !strings.HasPrefix(code, "<"):
		return "Đoạn mã phải là HTML và bắt đầu bằng thẻ, ví dụ <script>."
	}
	return ""
}
