package main

import (
	"net/http"
	"strings"
)

// adScriptMaxBytes keeps a paste inside the ad_settings.script TEXT column, which
// holds 65,535 bytes. A real ad tag is a few hundred.
const adScriptMaxBytes = 60000

// adsPage is the view model for GET /ads.
type adsPage struct {
	AdSettings
	Saved    bool
	Error    string
	MaxBytes int
}

// handleAdsPage shows the ad script and its on/off switch.
func (s *Server) handleAdsPage(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetAdSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	render(w, "ads.html", adsPage{
		AdSettings: settings,
		Saved:      r.URL.Query().Get("saved") == "1",
		MaxBytes:   adScriptMaxBytes,
	})
}

// handleSaveAds stores the script and the switch.
//
// The script runs unescaped on every visitor's page, so this POST is as sensitive
// as the SQL console's and needs the same sameOriginPost guard: basic auth is
// ambient, and a cross-site form could otherwise plant a script on the public site.
func (s *Server) handleSaveAds(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	enabled := r.FormValue("enabled") == "1"
	// Browsers submit a textarea with CRLF line endings.
	script := strings.TrimSpace(strings.ReplaceAll(r.FormValue("script"), "\r\n", "\n"))

	if msg := validateAdScript(enabled, script); msg != "" {
		// Re-render what was typed, so a rejected paste is not lost.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		render(w, "ads.html", adsPage{
			AdSettings: AdSettings{Enabled: enabled, Script: script},
			Error:      msg,
			MaxBytes:   adScriptMaxBytes,
		})
		return
	}
	if err := s.store.SaveAdSettings(r.Context(), enabled, script); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ads?saved=1", http.StatusSeeOther)
}

// validateAdScript returns why a save must be refused, or "" when it is fine. It
// catches paste mistakes, not malice: whoever passes basic auth is trusted with
// this field. The "<" check matters because JavaScript pasted without its <script>
// tags would print as visible text on every page of the public site.
func validateAdScript(enabled bool, script string) string {
	switch {
	case len(script) > adScriptMaxBytes:
		return "Đoạn mã quá dài."
	case script != "" && !strings.HasPrefix(script, "<"):
		return "Đoạn mã phải là HTML và bắt đầu bằng thẻ, ví dụ <script>."
	case enabled && script == "":
		return "Không thể bật quảng cáo khi chưa có đoạn mã."
	}
	return ""
}
