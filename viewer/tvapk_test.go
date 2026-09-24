package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// publishTVRelease writes an APK and a manifest the way CI does.
func publishTVRelease(t *testing.T, dir, file string, body []byte, mutate func(*tvRelease)) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	rel := tvRelease{
		VersionCode: 1002,
		VersionName: "0.1.2",
		File:        file,
		SHA256:      hex.EncodeToString(sum[:]),
		Size:        int64(len(body)),
		PublishedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
	}
	if mutate != nil {
		mutate(&rel)
	}
	raw, _ := json.Marshal(rel)
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func tvApkServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := &Server{tvApkDir: dir}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	return s
}

func get(s *Server, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://viewer.example"+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestTVApkDownload(t *testing.T) {
	dir := t.TempDir()
	body := []byte("PK\x03\x04 pretend this is a signed apk")
	publishTVRelease(t, dir, "phimnet-tv-0.1.2.apk", body, nil)
	s := tvApkServer(t, dir)

	w := get(s, "/download/phimnet-tv.apk")
	if w.Code != http.StatusOK || w.Body.String() != string(body) {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	// The type Android's package installer expects, and a file name with the
	// version in it for whoever saves it.
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.android.package-archive" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="phimnet-tv-0.1.2.apk"`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// A TV box on flaky Wi-Fi resumes rather than restarting.
	w = get(s, "/download/phimnet-tv.apk", "Range", "bytes=4-")
	if w.Code != http.StatusPartialContent || w.Body.String() != string(body[4:]) {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}

	// The stable URL's content changes between releases, so revalidation is by
	// content hash.
	etag := w.Header().Get("ETag")
	if w = get(s, "/download/phimnet-tv.apk", "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Fatalf("revalidation with the current ETag: %d", w.Code)
	}
}

func TestTVAppManifest(t *testing.T) {
	dir := t.TempDir()
	publishTVRelease(t, dir, "phimnet-tv-0.1.2.apk", []byte("apk"), nil)
	s := tvApkServer(t, dir)

	w := get(s, "/api/tv/v1/app")
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: %d %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["version_code"] != float64(1002) || got["version_name"] != "0.1.2" {
		t.Fatalf("manifest body: %v", got)
	}
	// Absolute: the app hands it to the system installer, not a browser.
	if got["download_url"] != "http://viewer.example/download/phimnet-tv.apk" {
		t.Fatalf("download_url = %v", got["download_url"])
	}
}

func TestTVApkBeforeFirstRelease(t *testing.T) {
	s := tvApkServer(t, t.TempDir())
	if w := get(s, "/download/phimnet-tv.apk"); w.Code != http.StatusNotFound {
		t.Fatalf("download with nothing published: %d", w.Code)
	}
	if w := get(s, "/api/tv/v1/app"); w.Code != http.StatusNotFound {
		t.Fatalf("manifest with nothing published: %d", w.Code)
	}
}

// The manifest names a file on disk, so it is validated, not trusted: a bad or
// tampered latest.json must never make the route serve something else.
func TestTVApkRejectsABadManifest(t *testing.T) {
	cases := map[string]func(*tvRelease){
		"path traversal":         func(r *tvRelease) { r.File = "../../../etc/passwd" },
		"not an apk":             func(r *tvRelease) { r.File = "latest.json" },
		"size does not match":    func(r *tvRelease) { r.Size++ },
		"missing version":        func(r *tvRelease) { r.VersionCode = 0 },
		"names a file not there": func(r *tvRelease) { r.File = "phimnet-tv-9.9.9.apk" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			publishTVRelease(t, dir, "phimnet-tv-0.1.2.apk", []byte("apk"), mutate)
			s := tvApkServer(t, dir)
			if w := get(s, "/download/phimnet-tv.apk"); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("download: %d %q", w.Code, w.Body.String())
			}
			if w := get(s, "/api/tv/v1/app"); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("manifest: %d", w.Code)
			}
		})
	}
}

// Unconfigured is off, the same rollback convention as accounts and billing.
func TestTVApkRoutesNeedAConfiguredDirectory(t *testing.T) {
	routes := []string{"GET /tv", "HEAD /tv", "GET /download/phimnet-tv.apk", "HEAD /download/phimnet-tv.apk", "GET /api/tv/v1/app", "GET /{locale}/tv-app"}
	found := routesOf(t, &Server{})
	for _, route := range routes {
		if found[route] {
			t.Errorf("%s must not be registered without TV_APK_DIR", route)
		}
	}
	found = routesOf(t, &Server{tvApkDir: t.TempDir()})
	for _, route := range routes {
		if !found[route] {
			t.Errorf("%s should be registered with TV_APK_DIR", route)
		}
	}
}

// Sideloaders like Downloader have the URL typed in with a TV remote, so the APK
// is also served at the short /tv — the same bytes and headers, not a redirect.
// And it answers HEAD, which download managers send first for the size.
func TestTVApkShortURLAndHead(t *testing.T) {
	dir := t.TempDir()
	body := []byte("PK\x03\x04 signed apk")
	publishTVRelease(t, dir, "phimnet-tv-0.1.2.apk", body, nil)
	s := tvApkServer(t, dir)

	w := get(s, "/tv")
	if w.Code != http.StatusOK || w.Body.String() != string(body) {
		t.Fatalf("/tv: %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.android.package-archive" {
		t.Fatalf("/tv Content-Type = %q", ct)
	}

	r := httptest.NewRequest(http.MethodHead, "http://viewer.example/tv", nil)
	hw := httptest.NewRecorder()
	s.ServeHTTP(hw, r)
	if hw.Code != http.StatusOK || hw.Body.Len() != 0 {
		t.Fatalf("HEAD /tv: %d, %d body bytes", hw.Code, hw.Body.Len())
	}
	if cl := hw.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", cl, len(body))
	}
}

// The install guide tells people what to type into Downloader, so it must show
// the absolute short URL, the release it will fetch, and the optional
// Downloader code — and the header links to it from every page.
func TestTVAppGuidePage(t *testing.T) {
	dir := t.TempDir()
	publishTVRelease(t, dir, "phimnet-tv-0.1.2.apk", []byte("PK\x03\x04 signed apk"), nil)
	s := tvApkServer(t, dir)
	s.tvDownloaderCode = "1234567"

	w := get(s, "/en/tv-app")
	if w.Code != http.StatusOK {
		t.Fatalf("/en/tv-app: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"http://viewer.example/tv",
		"Version 0.1.2",
		"1234567",
		`href="/download/phimnet-tv.apk"`,
		`href="/en/tv-app"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide page is missing %q", want)
		}
	}

	// Every page links the guide, not only the guide itself — from the header
	// and from the footer, since the header nav is hidden on phones.
	if page := get(s, "/en/bookmarks").Body.String(); strings.Count(page, `href="/en/tv-app"`) != 2 {
		t.Errorf("want the guide linked from header and footer, got %d links", strings.Count(page, `href="/en/tv-app"`))
	}
}

// Before the first release the guide still renders, but says so instead of
// walking someone through a download that would 404.
func TestTVAppGuideBeforeFirstRelease(t *testing.T) {
	s := tvApkServer(t, t.TempDir())
	w := get(s, "/en/tv-app")
	if w.Code != http.StatusOK {
		t.Fatalf("/en/tv-app: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "available for download yet") {
		t.Error("guide does not say the app is unpublished")
	}
	if strings.Contains(body, "Version ") || strings.Contains(body, `href="/download/phimnet-tv.apk"`) {
		t.Error("guide offers a download that does not exist")
	}
}

// Without TV_APK_DIR there is nothing to install: no guide, no header link.
func TestTVAppGuideNeedsAConfiguredDirectory(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	if w := get(s, "/en/tv-app"); w.Code != http.StatusNotFound {
		t.Fatalf("/en/tv-app without TV_APK_DIR: %d, want 404", w.Code)
	}
}
