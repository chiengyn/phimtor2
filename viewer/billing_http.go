package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	qrcode "github.com/skip2/go-qrcode"
)

// The browser-facing half of billing: the plans page, invoice creation, and the
// invoice page the buyer watches while their transfer confirms.

type planView struct {
	Code    string
	Name    string
	Kind    string
	Note    string
	USD     string
	VND     string
	Current bool // already owned — rendered but not buyable
}

type chainView struct {
	Name  string
	Label string
}

type plansData struct {
	Plans  []planView
	Chains []chainView

	SignedIn bool
	LoginURL string
	// HasPass is "already entitled to every 4K source", from a paid pass OR an
	// admin comp — the page only needs to know not to sell it again.
	HasPass   bool
	PassUntil string
	// PassForever suppresses the expiry date: a permanent admin comp is stored
	// as the maximum DATETIME, and telling someone their access runs out in the
	// year 9999 reads as a bug.
	PassForever bool

	// Set when arriving from a watch page (/vi/plans?title=123 or /en/plans): the per-title unlock
	// is only meaningful with a title in hand.
	TitleID   int64
	TitleName string
	Unlocked  bool
}

type invoiceData struct {
	Ref         string
	ProductName string
	PayTo       string
	PayAmount   string
	USD         string
	VND         string
	ChainLabel  string
	OtherChains []chainView
	QR          template.URL
	Status      string
	Paid        bool
	Expired     bool
	ExpiresAt   string
	BackHref    string
}

func (s *Server) handlePlansPage(w http.ResponseWriter, r *http.Request) {
	locale := localeFromContext(r.Context())
	u := userFrom(r.Context())
	now := time.Now()
	data := plansData{
		SignedIn: u != nil,
		LoginURL: s.loginURL(r),
		HasPass:  u.HasPass(now) || u.HasComp(now),
	}
	// Report whichever entitlement runs longest, and never print the sentinel
	// year a permanent comp is stored as.
	if until := u.EntitledUntil(now); until != nil {
		if until.Year() >= 9999 {
			data.PassForever = true
		} else {
			data.PassUntil = formatDate(locale, *until)
		}
	}
	for _, c := range s.billing.chains() {
		if wch, ok := s.billing.watcher(c); ok {
			data.Chains = append(data.Chains, chainView{Name: wch.Name(), Label: wch.Label()})
		}
	}

	// ?title= is optional: without it the page still sells the time passes, it
	// just cannot offer the per-title unlock (there is no title to unlock).
	if raw := r.URL.Query().Get("title"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if t, err := s.store.GetTitle(r.Context(), locale, id); err == nil && t != nil {
				data.TitleID = t.ID
				data.TitleName = t.Title
				if u != nil {
					if ok, err := s.store.HasTitleUnlock(r.Context(), u.ID, t.ID); err == nil {
						data.Unlocked = ok
					}
				}
			}
		}
	}

	for _, p := range s.billing.plans {
		if p.Kind == "title" && data.TitleID == 0 {
			continue // nothing to unlock
		}
		pv := planView{
			Code: p.Code, Kind: p.Kind,
			USD: usdString(p.Cents), VND: vndString(s.billing.vnd(p.Cents)),
		}
		switch p.Kind {
		case "pass":
			if p.Code == "pass365" {
				pv.Name = tr(locale, "plans.pass365")
			} else {
				pv.Name = tr(locale, "plans.pass30")
			}
			pv.Note = tr(locale, "plans.pass_note")
			pv.Current = data.HasPass
		case "title":
			pv.Name = tr(locale, "plans.title_unlock", data.TitleName)
			pv.Note = tr(locale, "plans.title_note")
			pv.Current = data.Unlocked
		}
		data.Plans = append(data.Plans, pv)
	}
	s.render(w, r, s.plans, data)
}

func (s *Server) handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	requestLocale, localeOK := parseLocale(r.Header.Get("X-Phimnet-Locale"))
	if !localeOK {
		requestLocale = LocaleVI
	}
	var body struct {
		PlanCode string `json:"plan_code"`
		TitleID  int64  `json:"title_id"`
		Chain    string `json:"chain"`
		Locale   string `json:"locale"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, tr(requestLocale, "billing.invalid_request"))
		return
	}
	invoiceLocale, ok := parseLocale(body.Locale)
	if !ok {
		invoiceLocale = requestLocale
	}
	p, ok := s.billing.planByCode(body.PlanCode)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, tr(invoiceLocale, "billing.invalid_plan"))
		return
	}
	if _, ok := s.billing.watcher(body.Chain); !ok {
		writeJSONError(w, http.StatusBadRequest, tr(invoiceLocale, "billing.invalid_chain"))
		return
	}

	var titleID *int64
	if p.Kind == "title" {
		if body.TitleID <= 0 {
			writeJSONError(w, http.StatusBadRequest, tr(invoiceLocale, "billing.missing_title"))
			return
		}
		t, err := s.store.GetTitle(r.Context(), invoiceLocale, body.TitleID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if t == nil {
			writeJSONError(w, http.StatusNotFound, tr(invoiceLocale, "billing.title_not_found"))
			return
		}
		// Refuse to sell something already owned. A pass is different — buying
		// another stacks onto the expiry, which is a legitimate renewal.
		already, err := s.store.HasTitleUnlock(r.Context(), u.ID, t.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if already {
			writeJSONError(w, http.StatusConflict, tr(invoiceLocale, "billing.already_unlocked"))
			return
		}
		titleID = &t.ID
	}

	inv, err := s.billing.createInvoice(r.Context(), u.ID, p, titleID, body.Chain)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": inv.Ref,
		"url": localeURL(invoiceLocale, "/payment/"+inv.Ref),
	})
}

// loadOwnedInvoice fetches an invoice and enforces ownership. A ref belonging to
// someone else is reported as MISSING, not forbidden — a 403 would confirm the
// ref exists, which is exactly what an enumerator wants to learn.
func (s *Server) loadOwnedInvoice(r *http.Request) (*Invoice, bool) {
	u := userFrom(r.Context())
	if u == nil {
		return nil, false
	}
	inv, err := s.store.InvoiceByRef(r.Context(), chi.URLParam(r, "ref"))
	if err != nil || inv == nil || inv.UserID != u.ID {
		return nil, false
	}
	return inv, true
}

func (s *Server) handleInvoicePage(w http.ResponseWriter, r *http.Request) {
	locale := localeFromContext(r.Context())
	if userFrom(r.Context()) == nil {
		// Anonymous: send them through sign-in and back, rather than 404ing a
		// page that would work a moment later.
		if lu := s.loginURL(r); lu != "" {
			http.Redirect(w, r, lu, http.StatusSeeOther)
			return
		}
		s.renderNotFound(w, r)
		return
	}
	inv, ok := s.loadOwnedInvoice(r)
	if !ok {
		s.renderNotFound(w, r)
		return
	}

	data := invoiceData{
		Ref:         inv.Ref,
		ProductName: s.productName(r, inv),
		PayTo:       inv.PayTo,
		PayAmount:   inv.PayAmount,
		USD:         usdString(inv.AmountUSDCents),
		VND:         vndString(s.billing.vnd(inv.AmountUSDCents)),
		Status:      inv.Status,
		Paid:        inv.Paid(),
		Expired:     inv.Status == "expired",
		ExpiresAt:   inv.ExpiresAt.Format("15:04") + " " + formatDate(locale, inv.ExpiresAt),
		BackHref:    localeURL(locale, "/plans"),
	}
	if wch, ok := s.billing.watcher(inv.Chain); ok {
		data.ChainLabel = wch.Label()
	}
	// Every EVM chain shares the receive address and the amount namespace, so the
	// buyer may pay on whichever is cheapest for them and still be credited.
	for _, c := range s.billing.chains() {
		if c == inv.Chain {
			continue
		}
		if wch, ok := s.billing.watcher(c); ok && wch.PayTo() == inv.PayTo {
			data.OtherChains = append(data.OtherChains, chainView{Name: wch.Name(), Label: wch.Label()})
		}
	}
	if png, err := qrcode.Encode(inv.PayTo, qrcode.Medium, 260); err == nil {
		data.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
	}
	s.render(w, r, s.invoice, data)
}

// handleInvoiceStatus backs the invoice page's poll. The page must not have to
// wait on the server-side ticker's next tick to learn it was paid.
func (s *Server) handleInvoiceStatus(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.loadOwnedInvoice(r)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "không tìm thấy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  inv.Status,
		"paid":    inv.Paid(),
		"expired": inv.Status == "expired",
	})
}

func (s *Server) productName(r *http.Request, inv *Invoice) string {
	locale := localeFromContext(r.Context())
	if p, ok := s.billing.planByCode(inv.PlanCode); ok && p.Kind == "pass" {
		if p.Code == "pass365" {
			return tr(locale, "plans.pass365")
		}
		return tr(locale, "plans.pass30")
	}
	if inv.TitleID != nil {
		if t, err := s.store.GetTitle(r.Context(), localeFromContext(r.Context()), *inv.TitleID); err == nil && t != nil {
			return tr(locale, "plans.title_unlock", t.Title)
		}
	}
	return tr(locale, "plans.title")
}

func usdString(cents int) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// vndString renders whole dong with thousands separators, the way prices are
// written in Vietnam ("52.000 ₫").
func vndString(v int) string {
	if v <= 0 {
		return ""
	}
	s := strconv.Itoa(v)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ".") + " ₫"
}
