package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// adRefreshInterval is how long a change saved at the admin's /ads can take
	// to reach the site.
	adRefreshInterval = 30 * time.Second
	adRefreshTimeout  = 5 * time.Second
)

// adSlotKeys are the banner slots the templates place with {{adSlot "key"}}. The
// admin's adSlotDefs (admin/ads.go) lists the same keys, with the copy that tells
// an operator where each one sits. The lists are duplicated by convention: a key
// only the admin knows is never placed, and an unknown key in a template is a
// render error, which TestAdSlotKeysArePlaced catches.
var adSlotKeys = []string{"home_top", "home_feed", "detail", "watch", "footer"}

// adMarker is what {{adSlot "key"}} prints. render replaces every marker in the
// finished page, with the slot's code or with nothing. Page text cannot forge
// one: html/template escapes "<" in anything that is not template.HTML, and the
// only template.HTML values are admin-written.
func adMarker(key string) string { return "<!--phimnet-ad:" + key + "-->" }

// adSlotMarker is the template func behind {{adSlot "key"}}.
func adSlotMarker(key string) (template.HTML, error) {
	if !slices.Contains(adSlotKeys, key) {
		return "", fmt.Errorf("unknown ad slot %q", key)
	}
	return template.HTML(adMarker(key)), nil
}

// adStrip removes every marker: the replacer for an ad-free visitor, for a
// page rendered before the first load, and for ads switched off.
var adStrip = func() *strings.Replacer {
	pairs := make([]string, 0, 2*len(adSlotKeys))
	for _, key := range adSlotKeys {
		pairs = append(pairs, adMarker(key), "")
	}
	return strings.NewReplacer(pairs...)
}()

// adMobileQuery is the media query under which a slot shows its mobile code.
// The admin's adMobileMaxWidth repeats the number for its page copy. A 728px
// banner plus the page gutter needs about 800px.
const adMobileQuery = "(max-width: 800px)"

// AdConfig is what the admin saved, as far as the site is concerned: empty while
// the master switch is off, and only the enabled slots with code.
type AdConfig struct {
	Script string
	Slots  map[string]AdSlotCode
}

// AdSlotCode is one slot's banner code. Mobile, when set, replaces Desktop on
// narrow screens. When it is empty, phones get Desktop too.
type AdSlotCode struct {
	Desktop string
	Mobile  string
}

// adSlotMarkup is what replaces a slot's marker.
//
// With one code it is printed as is. With a mobile code too, the choice has to
// be made in the browser, by screen width, while the page is still parsing: a
// small script document.writes the matching variant in place, so only that
// one ever loads, counts an impression or runs its own document.write calls.
// Rendering both and hiding one with CSS would load and count both. Writing the
// wrapper along with the code means a device with no code for this slot gets no
// empty box. json.Marshal escapes "<", so code containing </script> cannot end
// the outer script early.
func adSlotMarkup(key string, c AdSlotCode) string {
	wrap := func(device, code string) string {
		if code == "" {
			return ""
		}
		return `<div class="ad-slot" data-ad-slot="` + html.EscapeString(key) + `" data-ad-device="` + device + `">` + code + `</div>`
	}
	if c.Mobile == "" {
		return wrap("all", c.Desktop)
	}
	mobile, _ := json.Marshal(wrap("mobile", c.Mobile))
	desktop, _ := json.Marshal(wrap("desktop", c.Desktop))
	query, _ := json.Marshal(adMobileQuery)
	return `<script>document.write(matchMedia(` + string(query) + `).matches?` + string(mobile) + `:` + string(desktop) + `)</script>`
}

// adSnapshot is one loaded AdConfig, ready to print.
type adSnapshot struct {
	script template.HTML
	fill   *strings.Replacer
}

func newAdSnapshot(cfg AdConfig) *adSnapshot {
	pairs := make([]string, 0, 2*len(adSlotKeys))
	for _, key := range adSlotKeys {
		pairs = append(pairs, adMarker(key), adSlotMarkup(key, cfg.Slots[key]))
	}
	return &adSnapshot{script: template.HTML(cfg.Script), fill: strings.NewReplacer(pairs...)}
}

// adCache holds the ads the admin configured, so rendering a page never queries
// the database for them. A nil cache, like an empty config, means no ads.
type adCache struct {
	load    func(context.Context) (AdConfig, error)
	current atomic.Pointer[adSnapshot]
}

func newAdCache(load func(context.Context) (AdConfig, error)) *adCache {
	return &adCache{load: load}
}

// script returns the global markup printed once per page. Converting to
// template.HTML turns off escaping, which is what an ad tag needs. It is safe
// only because the admin's basic-auth /ads page is the sole writer of this value.
func (c *adCache) script() template.HTML {
	if c == nil {
		return ""
	}
	if p := c.current.Load(); p != nil {
		return p.script
	}
	return ""
}

// slots returns the replacer that fills a rendered page's slot markers. Same
// trust argument as script.
func (c *adCache) slots() *strings.Replacer {
	if c == nil {
		return adStrip
	}
	if p := c.current.Load(); p != nil {
		return p.fill
	}
	return adStrip
}

// refresh loads the settings once. A failed read keeps the last known value, so
// a database blip neither starts nor stops ads.
func (c *adCache) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, adRefreshTimeout)
	defer cancel()
	cfg, err := c.load(ctx)
	if err != nil {
		log.Printf("ads: load settings: %v", err)
		return
	}
	c.current.Store(newAdSnapshot(cfg))
}

// run keeps the cache fresh until ctx is cancelled.
func (c *adCache) run(ctx context.Context) {
	if c == nil {
		return
	}
	c.refresh(ctx)
	ticker := time.NewTicker(adRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}
