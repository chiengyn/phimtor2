package main

import (
	"context"
	"html/template"
	"log"
	"sync/atomic"
	"time"
)

const (
	// adRefreshInterval is how long a change saved at the admin's /ads can take
	// to reach the site.
	adRefreshInterval = 30 * time.Second
	adRefreshTimeout  = 5 * time.Second
)

// adCache holds the ad script the admin configured, so rendering a page never
// queries the database for it. A nil cache, like an empty script, means no ads.
type adCache struct {
	load    func(context.Context) (string, error)
	current atomic.Pointer[string]
}

func newAdCache(load func(context.Context) (string, error)) *adCache {
	return &adCache{load: load}
}

// script returns the markup to print into a page. Converting to template.HTML
// turns off escaping, which is what an ad tag needs. It is safe only because the
// admin's basic-auth /ads page is the sole writer of this value.
func (c *adCache) script() template.HTML {
	if c == nil {
		return ""
	}
	if p := c.current.Load(); p != nil {
		return template.HTML(*p)
	}
	return ""
}

// refresh loads the setting once. A failed read keeps the last known value, so a
// database blip neither starts nor stops ads.
func (c *adCache) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, adRefreshTimeout)
	defer cancel()
	script, err := c.load(ctx)
	if err != nil {
		log.Printf("ads: load settings: %v", err)
		return
	}
	c.current.Store(&script)
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
