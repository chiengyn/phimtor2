package main

// Serving the Android TV app.
//
// A television cannot install this app from Play, so the viewer hands it out:
// a stable download URL for people (GET /download/phimnet-tv.apk) and a small
// manifest the installed app can poll for updates (GET /api/tv/v1/app).
//
// The viewer never builds or writes any of it. CI (.github/workflows/android.yml)
// signs the APK, copies it into TV_APK_DIR on the host — mounted read-only into
// this container — and only THEN replaces latest.json with a single atomic
// rename. So the manifest is the one source of truth for "which APK is
// current", and it can never name a file that is still uploading. Keeping the
// APK out of the viewer image means a new app version needs no viewer deploy,
// and a viewer deploy needs no Android build.
//
// Rolling back is editing latest.json to name an older APK: CI keeps the last
// few next to it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// tvRelease is latest.json, as CI writes it.
type tvRelease struct {
	VersionCode int       `json:"version_code"`
	VersionName string    `json:"version_name"`
	File        string    `json:"file"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	PublishedAt time.Time `json:"published_at"`
	Commit      string    `json:"commit,omitempty"`
}

// tvApkName is the only shape of file name the manifest may point at. The
// manifest names a file on disk, so without this a bad (or tampered)
// latest.json — "../../etc/passwd" — would turn the download route into an
// arbitrary file read. It is checked, not trusted.
var tvApkName = regexp.MustCompile(`^phimnet-tv-[0-9A-Za-z._-]+\.apk$`)

var errNoTVRelease = errors.New("no TV app release has been published")

// loadTVRelease reads and validates the current manifest. It is read on every
// request rather than cached: the file is tiny, and not caching is what lets a
// new release (or a rollback) take effect the instant CI renames it into place.
func loadTVRelease(dir string) (*tvRelease, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "latest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoTVRelease
	}
	if err != nil {
		return nil, err
	}
	var rel tvRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, fmt.Errorf("latest.json: %w", err)
	}
	if !tvApkName.MatchString(rel.File) {
		return nil, fmt.Errorf("latest.json names %q, which is not an APK this service will serve", rel.File)
	}
	if rel.VersionCode <= 0 || rel.VersionName == "" || len(rel.SHA256) != 64 {
		return nil, errors.New("latest.json is missing version_code, version_name or sha256")
	}
	// The manifest must describe the file that is actually there, or the app's
	// update check would promise one build while the download delivered another.
	st, err := os.Stat(filepath.Join(dir, rel.File))
	if err != nil {
		return nil, fmt.Errorf("latest.json names %q: %w", rel.File, err)
	}
	if st.Size() != rel.Size {
		return nil, fmt.Errorf("latest.json says %q is %d bytes, it is %d", rel.File, rel.Size, st.Size())
	}
	return &rel, nil
}

// handleTVApk serves the current APK under a stable URL.
//
// http.ServeContent gives it Range support, which matters: a TV box on flaky
// Wi-Fi resumes a download rather than restarting it. The ETag is the APK's
// SHA-256, and no-cache makes every client revalidate — the URL never changes
// while its content does, so a cached copy must never be served as current.
func (s *Server) handleTVApk(w http.ResponseWriter, r *http.Request) {
	rel, err := loadTVRelease(s.tvApkDir)
	if errors.Is(err, errNoTVRelease) {
		http.Error(w, "The TV app has not been published yet.", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("tv apk: %v", err)
		http.Error(w, "The TV app is temporarily unavailable.", http.StatusServiceUnavailable)
		return
	}
	f, err := os.Open(filepath.Join(s.tvApkDir, rel.File))
	if err != nil {
		log.Printf("tv apk: open %s: %v", rel.File, err)
		http.Error(w, "The TV app is temporarily unavailable.", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Content-Type", "application/vnd.android.package-archive")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="phimnet-tv-%s.apk"`, rel.VersionName))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", `"`+rel.SHA256+`"`)
	// An empty name stops ServeContent guessing a type from the extension; the
	// Content-Type above is the one Android's package installer expects.
	http.ServeContent(w, r, "", rel.PublishedAt, f)
}

// handleTVAppRelease is the update manifest: the installed app compares
// version_code with its own. The download URL is absolute (absoluteFor) because
// the app hands it to the system's installer, not to a browser.
func (s *Server) handleTVAppRelease(w http.ResponseWriter, r *http.Request) {
	rel, err := loadTVRelease(s.tvApkDir)
	if errors.Is(err, errNoTVRelease) {
		writeJSONError(w, http.StatusNotFound, "no release published")
		return
	}
	if err != nil {
		log.Printf("tv app release: %v", err)
		writeJSONError(w, http.StatusServiceUnavailable, "release unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version_code": rel.VersionCode,
		"version_name": rel.VersionName,
		"sha256":       rel.SHA256,
		"size":         rel.Size,
		"published_at": rel.PublishedAt,
		"download_url": s.absoluteFor(r, "/download/phimnet-tv.apk"),
	})
}
