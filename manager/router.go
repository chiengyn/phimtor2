package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// errNoInstance is returned by placeAdd when no healthy streamer is available.
var errNoInstance = errors.New("no streamer instance available")

// torrentEntry is the manager's view of a streamer torrent: the streamer's own
// fields passed through, plus the owning streamer's public URL the browser needs
// for stats/stream.
type torrentEntry map[string]any

// listInfohashes fetches just the infohashes a streamer currently holds (used by
// reconcile).
func (r *Registry) listInfohashes(ctx context.Context, in *Instance) ([]string, error) {
	var torrents []struct {
		InfoHash string `json:"infoHash"`
	}
	if err := r.getJSON(ctx, in, "/api/torrents", &torrents); err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(torrents))
	for _, t := range torrents {
		hashes = append(hashes, t.InfoHash)
	}
	return hashes, nil
}

// listTorrents fetches a streamer's full torrent list as generic maps, annotated
// with its public URL.
func (r *Registry) listTorrents(ctx context.Context, in *Instance) ([]torrentEntry, error) {
	var torrents []torrentEntry
	if err := r.getJSON(ctx, in, "/api/torrents", &torrents); err != nil {
		return nil, err
	}
	for _, t := range torrents {
		t["streamerPublicURL"] = in.PublicURL
	}
	return torrents, nil
}

// aggregateList fans out a list to every healthy instance and merges the
// results. A slow or failing instance is skipped, not fatal — the list is
// best-effort, matching how the consumers already tolerate partial results.
func (r *Registry) aggregateList(ctx context.Context) []torrentEntry {
	out := []torrentEntry{}
	for _, in := range r.healthyInstances() {
		torrents, err := r.listTorrents(ctx, in)
		if err != nil {
			continue
		}
		out = append(out, torrents...)
	}
	return out
}

// errInstanceNotFound is returned when an add/list targets an unknown instance.
var errInstanceNotFound = errors.New("streamer instance not found")

// placeAdd picks a streamer (least-loaded), forwards the add, records ownership,
// and returns the new infohash plus the owning streamer's public URL.
func (r *Registry) placeAdd(ctx context.Context, contentType string, body []byte, magnet string) (map[string]string, error) {
	// Dedupe: if we can read the infohash off the magnet and already own it on a
	// healthy instance, return that instead of placing a second copy.
	if res := r.dedupeAdd(magnet); res != nil {
		return res, nil
	}
	in := r.pickInstance()
	if in == nil {
		return nil, errNoInstance
	}
	return r.addVia(ctx, in, contentType, body)
}

// addToInstance forwards an add to a specific streamer (bypassing load
// balancing) — used by the per-streamer watch page.
func (r *Registry) addToInstance(ctx context.Context, id, contentType string, body []byte, magnet string) (map[string]string, error) {
	in, ok := r.instanceByID(id)
	if !ok || !in.healthy(r.ttl) {
		return nil, errInstanceNotFound
	}
	// Dedupe only when the magnet already lives on THIS instance; otherwise place
	// it here as requested.
	if hash := infohashFromMagnet(magnet); hash != "" {
		if owner, ok := r.owner(hash); ok && owner.ID == in.ID && owner.healthy(r.ttl) {
			return map[string]string{"infoHash": hash, "streamerPublicURL": in.PublicURL}, nil
		}
	}
	return r.addVia(ctx, in, contentType, body)
}

// dedupeAdd returns an existing placement for a magnet's infohash if one is
// already owned by a healthy instance, else nil.
func (r *Registry) dedupeAdd(magnet string) map[string]string {
	if hash := infohashFromMagnet(magnet); hash != "" {
		if in, ok := r.owner(hash); ok && in.healthy(r.ttl) {
			return map[string]string{"infoHash": hash, "streamerPublicURL": in.PublicURL}
		}
	}
	return nil
}

// addVia forwards an add to the given instance and records ownership.
func (r *Registry) addVia(ctx context.Context, in *Instance, contentType string, body []byte) (map[string]string, error) {
	resp, err := in.do(ctx, http.MethodPost, "/api/torrents", contentType, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("streamer add failed: " + resp.Status)
	}

	var added struct {
		InfoHash string `json:"infoHash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil || added.InfoHash == "" {
		return nil, errors.New("streamer returned no infohash")
	}

	r.setOwner(added.InfoHash, in)
	return map[string]string{"infoHash": added.InfoHash, "streamerPublicURL": in.PublicURL}, nil
}

// getTorrent resolves the owner and returns the torrent info annotated with the
// streamer's public URL. ok=false means no instance has it.
func (r *Registry) getTorrent(ctx context.Context, hash string) (torrentEntry, bool) {
	in, ok := r.resolveOwner(ctx, hash)
	if !ok {
		return nil, false
	}
	var entry torrentEntry
	if err := r.getJSON(ctx, in, "/api/torrents/"+hash, &entry); err != nil {
		// The owner 404'd or errored between resolve and fetch; treat as gone.
		r.clearOwner(hash)
		return nil, false
	}
	entry["streamerPublicURL"] = in.PublicURL
	return entry, true
}

// getMetainfo fetches the bencoded .torrent from the streamer that owns the
// torrent. It returns the streamer's status code so the caller can tell "not
// ready yet" (409, metadata still resolving) from "gone" (404, no owner): only a
// 200 carries bytes.
func (r *Registry) getMetainfo(ctx context.Context, hash string) ([]byte, int) {
	in, ok := r.resolveOwner(ctx, hash)
	if !ok {
		return nil, http.StatusNotFound
	}
	resp, err := in.do(ctx, http.MethodGet, "/api/torrents/"+hash+"/metainfo", "", nil)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, resp.StatusCode
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAddBody))
	if err != nil {
		return nil, http.StatusBadGateway
	}
	return data, http.StatusOK
}

// deleteTorrent routes a delete to the owner. It is idempotent: a torrent no
// instance owns is already in the desired state, so this still succeeds.
func (r *Registry) deleteTorrent(ctx context.Context, hash string) error {
	in, ok := r.resolveOwner(ctx, hash)
	if !ok {
		return nil
	}
	resp, err := in.do(ctx, http.MethodDelete, "/api/torrents/"+hash, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r.clearOwner(hash)
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("streamer delete failed: " + resp.Status)
	}
	return nil
}

// resolveOwner returns the instance owning a torrent. It trusts the owner map,
// but verifies on a probe when the map misses or the cached owner no longer has
// it — the self-heal that keeps the map honest against the idle reaper.
func (r *Registry) resolveOwner(ctx context.Context, hash string) (*Instance, bool) {
	if in, ok := r.owner(hash); ok && in.healthy(r.ttl) {
		if r.instanceHas(ctx, in, hash) {
			return in, true
		}
	}
	return r.reResolve(ctx, hash)
}

// reResolve probes every healthy instance for the torrent and updates the map
// with whoever has it. Clears the map entry if nobody does.
func (r *Registry) reResolve(ctx context.Context, hash string) (*Instance, bool) {
	for _, in := range r.healthyInstances() {
		if r.instanceHas(ctx, in, hash) {
			r.setOwner(hash, in)
			return in, true
		}
	}
	r.clearOwner(hash)
	return nil, false
}

// instanceHas reports whether a streamer currently holds the torrent.
func (r *Registry) instanceHas(ctx context.Context, in *Instance, hash string) bool {
	resp, err := in.do(ctx, http.MethodGet, "/api/torrents/"+hash, "", nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// getJSON does a GET against a streamer's internal API and decodes the body.
func (r *Registry) getJSON(ctx context.Context, in *Instance, path string, v any) error {
	resp, err := in.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("unexpected status " + resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

var btihHexRe = regexp.MustCompile(`(?i)urn:btih:([0-9a-f]{40})`)

// infohashFromMagnet pulls a lowercase hex infohash from a magnet URI, or ""
// if absent or base32-encoded (dedupe is best-effort; a miss just skips it).
func infohashFromMagnet(magnet string) string {
	if magnet == "" {
		return ""
	}
	if u, err := url.Parse(magnet); err == nil {
		for _, xt := range u.Query()["xt"] {
			if m := btihHexRe.FindStringSubmatch(xt); m != nil {
				return strings.ToLower(m[1])
			}
		}
	}
	if m := btihHexRe.FindStringSubmatch(magnet); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}
