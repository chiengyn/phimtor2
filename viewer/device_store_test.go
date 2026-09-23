package main

// Integration tests for the device-pairing queries.
//
// These need a real MySQL with the admin's migrations applied, because the whole
// point of what they check is behaviour the database owns and Go cannot fake:
// the UNIQUE/NULL interaction that lets a settled user_code return to
// circulation, and the status guards that make approval idempotent under a
// double submit.
//
// Skipped unless PHIMTOR_TEST_DSN is set, so the ordinary `go test ./...` stays
// hermetic. Run them with, e.g.:
//
//	PHIMTOR_TEST_DSN='phimtor:phimtor@tcp(127.0.0.1:3306)/phimtor?parseTime=true&charset=utf8mb4' go test -run TestDevice.*Integration ./...

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testStore opens the integration database, or skips. It also registers cleanup
// of every row these tests create, keyed by the device_name marker below, so a
// run leaves the database as it found it.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PHIMTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("PHIMTOR_TEST_DSN not set; skipping database integration test")
	}
	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec(`DELETE FROM tv_devices WHERE device_name = ?`, testDeviceMarker); err != nil {
			t.Logf("cleanup: %v", err)
		}
		store.Close()
	})
	return store
}

const testDeviceMarker = "go-test-device"

// testUserID creates (or reuses) a throwaway account, because tv_devices.user_id
// is a real foreign key. It is left behind deliberately: deleting it would
// cascade, and a stable row keeps reruns cheap.
func testUserID(t *testing.T, store *Store) int64 {
	t.Helper()
	const uid = "go-test-provider-uid"
	var id int64
	err := store.db.QueryRow(`SELECT id FROM users WHERE provider = 'google' AND provider_uid = ?`, uid).Scan(&id)
	if err == nil {
		return id
	}
	if err != sql.ErrNoRows {
		t.Fatalf("look up test user: %v", err)
	}
	res, err := store.db.Exec(
		`INSERT INTO users (provider, provider_uid, email, name) VALUES ('google', ?, 'go-test@example.invalid', 'Go Test')`, uid)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		t.Fatalf("test user id: %v", err)
	}
	return id
}

func TestDevicePairingLifecycleIntegration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := testUserID(t, store)

	deviceCode, userCode := "dc-lifecycle-"+t.Name(), "ACDEFGHJ"
	id, err := store.CreateDevicePairing(ctx, deviceCode, userCode, testDeviceMarker, devicePairingTTL)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Pending: the TV polls and must be told to keep waiting.
	pairing, err := store.DevicePairing(ctx, deviceCode)
	if err != nil || pairing == nil {
		t.Fatalf("read back pending pairing: %v (%v)", err, pairing)
	}
	if pairing.Status != "pending" || pairing.Expired || pairing.ID != id {
		t.Fatalf("unexpected pending pairing: %+v", pairing)
	}

	// A token minted now would name a device that is not approved, so the
	// middleware must refuse it.
	if active, err := store.DeviceActive(ctx, id, userID); err != nil || active {
		t.Fatalf("a pending device must not be active (active=%v err=%v)", active, err)
	}

	// Approve from the phone.
	ok, err := store.ApproveDevicePairing(ctx, userCode, userID)
	if err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if active, err := store.DeviceActive(ctx, id, userID); err != nil || !active {
		t.Fatalf("an approved device must be active (active=%v err=%v)", active, err)
	}

	// The TV's next poll now gets a token.
	pairing, err = store.DevicePairing(ctx, deviceCode)
	if err != nil || pairing == nil {
		t.Fatalf("read back approved pairing: %v", err)
	}
	if pairing.Status != "approved" || pairing.UserID != userID {
		t.Fatalf("unexpected approved pairing: %+v", pairing)
	}

	// Revoke: "sign out this television". The token still verifies its MAC —
	// this row is the only thing that stops it.
	if err := store.RevokeDevice(ctx, id, userID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if active, err := store.DeviceActive(ctx, id, userID); err != nil || active {
		t.Fatalf("a revoked device must not be active (active=%v err=%v)", active, err)
	}
}

// Approval is guarded by `status = 'pending'`, so a replayed submit collapses to
// zero rows affected rather than re-binding the television.
func TestApproveDevicePairingIsSingleUseIntegration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := testUserID(t, store)

	const userCode = "ACDEFGHK"
	if _, err := store.CreateDevicePairing(ctx, "dc-single-use", userCode, testDeviceMarker, devicePairingTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := store.ApproveDevicePairing(ctx, userCode, userID); err != nil || !ok {
		t.Fatalf("first approve: ok=%v err=%v", ok, err)
	}
	if ok, err := store.ApproveDevicePairing(ctx, userCode, userID); err != nil || ok {
		t.Fatalf("second approve must be a no-op: ok=%v err=%v", ok, err)
	}
}

// The reason user_code is NULLable rather than merely UNIQUE: MySQL treats NULLs
// as DISTINCT in a unique index, so NULLing on approval returns the code to
// circulation. Without that, every pairing would permanently burn one code out
// of a deliberately small alphabet.
func TestUserCodeReturnsToCirculationIntegration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := testUserID(t, store)

	const userCode = "ACDEFGHM"
	if _, err := store.CreateDevicePairing(ctx, "dc-recycle-1", userCode, testDeviceMarker, devicePairingTTL); err != nil {
		t.Fatalf("create first: %v", err)
	}

	// While that pairing is OPEN the code is taken, so a second one collides —
	// which is exactly the duplicate the handler retries on.
	_, err := store.CreateDevicePairing(ctx, "dc-recycle-2", userCode, testDeviceMarker, devicePairingTTL)
	if err == nil {
		t.Fatal("two open pairings were allowed to share a user_code")
	}
	if !isDuplicateKey(err) {
		t.Fatalf("collision surfaced as %v, not a duplicate-key error the handler retries on", err)
	}

	// Settle the first. The code should now be free.
	if ok, err := store.ApproveDevicePairing(ctx, userCode, userID); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if _, err := store.CreateDevicePairing(ctx, "dc-recycle-2", userCode, testDeviceMarker, devicePairingTTL); err != nil {
		t.Fatalf("user_code did not return to circulation after approval: %v", err)
	}
}

// Expiry is evaluated by MySQL (expires_at <= NOW()), never by comparing a Go
// time.Time, so clock skew between the app and the database cannot expire a code
// early. A negative TTL is the cheapest way to assert the comparison happens.
func TestExpiredPairingIntegration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := testUserID(t, store)

	const userCode = "ACDEFGHN"
	if _, err := store.CreateDevicePairing(ctx, "dc-expired", userCode, testDeviceMarker, -time.Minute); err != nil {
		t.Fatalf("create: %v", err)
	}
	pairing, err := store.DevicePairing(ctx, "dc-expired")
	if err != nil || pairing == nil {
		t.Fatalf("read back: %v", err)
	}
	if !pairing.Expired {
		t.Fatal("a pairing whose expires_at has passed did not report Expired")
	}
	// An expired code must not be approvable, however it is typed.
	if ok, err := store.ApproveDevicePairing(ctx, userCode, userID); err != nil || ok {
		t.Fatalf("an expired pairing was approved: ok=%v err=%v", ok, err)
	}
	// And the sweep should remove it.
	if err := store.ReapDevicePairings(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if pairing, err := store.DevicePairing(ctx, "dc-expired"); err != nil || pairing != nil {
		t.Fatalf("reap left the expired pairing behind: %+v (%v)", pairing, err)
	}
}

// A revoked row must not honour a token minted for a DIFFERENT account, which is
// why DeviceActive re-checks user_id rather than trusting the token's claim.
func TestDeviceActiveIsScopedToItsUserIntegration(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := testUserID(t, store)

	const userCode = "ACDEFGHP"
	id, err := store.CreateDevicePairing(ctx, "dc-scoped", userCode, testDeviceMarker, devicePairingTTL)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := store.ApproveDevicePairing(ctx, userCode, userID); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if active, err := store.DeviceActive(ctx, id, userID+99999); err != nil || active {
		t.Fatalf("device was active for the wrong user (active=%v err=%v)", active, err)
	}
	// Revocation is scoped the same way: another user's id must not unpair it.
	if err := store.RevokeDevice(ctx, id, userID+99999); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if active, err := store.DeviceActive(ctx, id, userID); err != nil || !active {
		t.Fatal("another user's revoke unpaired this device")
	}
}

// testTVServer builds a Server wired to the integration database, with accounts
// "configured" (dummy Google credentials) so the pairing routes register. No
// Google endpoint is ever contacted: pairing is precisely the flow that does not
// involve one.
func testTVServer(t *testing.T) *Server {
	t.Helper()
	store := testStore(t)
	sess, _, err := newSessionSigner(strings.Repeat("s", minSecretLen), false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:           store,
		sess:            sess,
		google:          newGoogleClient("test-client-id", "test-secret", "http://localhost/cb"),
		deviceApprovals: newRateLimiter(deviceApprovalsPerHour, time.Hour),
	}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	s.setupRouter()
	return s
}

func postJSON(t *testing.T, s *Server, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The whole pairing round trip over real HTTP against a real database: the TV
// asks for a code, gets told to wait, the phone approves it, the TV receives a
// bearer token, and that token then authenticates an ordinary API request.
//
// This is the end-to-end path the plan called for, and the thing unit tests
// cannot reach — it crosses the router, both middlewares, the signer and MySQL.
func TestDevicePairingOverHTTPIntegration(t *testing.T) {
	s := testTVServer(t)
	userID := testUserID(t, s.store)

	// 1. The television opens a pairing.
	w := postJSON(t, s, "/api/tv/v1/device/code", `{"device_name":"`+testDeviceMarker+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("device/code: %d %s", w.Code, w.Body)
	}
	var codes struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &codes); err != nil {
		t.Fatal(err)
	}
	if codes.DeviceCode == "" || codes.Interval <= 0 {
		t.Fatalf("device/code returned an unusable response: %s", w.Body)
	}
	// The displayed code is grouped for reading across a room.
	if len(codes.UserCode) != userCodeLen+1 || codes.UserCode[4] != '-' {
		t.Fatalf("user_code %q is not in XXXX-XXXX form", codes.UserCode)
	}

	// 2. The TV polls before anyone has approved it.
	w = postJSON(t, s, "/api/tv/v1/device/token", `{"device_code":"`+codes.DeviceCode+`"}`, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "authorization_pending") {
		t.Fatalf("expected authorization_pending, got %d %s", w.Code, w.Body)
	}

	// 3. An ANONYMOUS approve must be refused — requireUser is the real gate.
	w = postJSON(t, s, "/api/tv/v1/device/approve", `{"user_code":"`+codes.UserCode+`"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approve should be 401, got %d %s", w.Code, w.Body)
	}

	// 4. The phone approves it, signed in. Typed in lower case with the hyphen,
	//    the way someone actually would.
	session := httptest.NewRecorder()
	s.sess.setSession(session, userID)
	cookie := session.Result().Cookies()[0]
	typed := strings.ToLower(codes.UserCode)
	w = postJSON(t, s, "/api/tv/v1/device/approve", `{"user_code":"`+typed+`"}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}

	// 5. The TV's next poll gets its token.
	w = postJSON(t, s, "/api/tv/v1/device/token", `{"device_code":"`+codes.DeviceCode+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.AccessToken == "" || issued.TokenType != "Bearer" {
		t.Fatalf("unusable token response: %s", w.Body)
	}

	// 6. That token authenticates an ordinary request — the point of the whole
	//    exercise. currentUser resolved it via readBearer plus the device row.
	get := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/tv/v1/me", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w = get(issued.AccessToken)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signed_in":true`) {
		t.Fatalf("bearer token did not authenticate: %d %s", w.Code, w.Body)
	}

	// Anonymous /me is a 200 with signed_in false, not a 401 — browsing without
	// an account is supported and the app must not treat it as an error.
	if w := get(""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signed_in":false`) {
		t.Fatalf("anonymous /me should be 200 signed_in=false, got %d %s", w.Code, w.Body)
	}

	// 7. Sign the television out. The token still verifies its MAC, so the
	//    device row is the only thing revoking it — which is why it exists.
	var deviceID int64
	if err := s.store.db.QueryRow(
		`SELECT id FROM tv_devices WHERE device_code = ?`, codes.DeviceCode).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	if err := s.store.RevokeDevice(context.Background(), deviceID, userID); err != nil {
		t.Fatal(err)
	}
	if w := get(issued.AccessToken); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signed_in":false`) {
		t.Fatalf("a revoked device was still authenticated: %d %s", w.Code, w.Body)
	}
}

// seedGatedTitle attaches one 720p, one 1080p and one 2160p source to an
// existing title, so the three quality tiers are all in play, and removes them
// again afterwards. It picks a title rather than creating one because the
// catalog is the admin's to write and the shapes there are not this test's
// concern.
func seedGatedTitle(t *testing.T, store *Store) int64 {
	t.Helper()
	var titleID int64
	if err := store.db.QueryRow(`SELECT id FROM titles ORDER BY id LIMIT 1`).Scan(&titleID); err != nil {
		t.Skipf("no titles in the test catalog: %v", err)
	}
	res, err := store.db.Exec(
		`INSERT INTO torrent_sources (info_hash, magnet) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
		"0000000000000000000000000000000000000001", "magnet:?xt=urn:btih:test")
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	sourceID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range []string{"720p", "1080p", "2160p"} {
		if _, err := store.db.Exec(
			`INSERT INTO videos (source_id, title_id, name, resolution, file_index, file_path, file_size)
			 VALUES (?, ?, ?, ?, ?, ?, 1)`,
			sourceID, titleID, "go-test-"+res, res, i, "/go-test/"+res+".mkv"); err != nil {
			t.Fatalf("seed video %s: %v", res, err)
		}
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec(`DELETE FROM videos WHERE name LIKE 'go-test-%'`); err != nil {
			t.Logf("cleanup videos: %v", err)
		}
		if _, err := store.db.Exec(`DELETE FROM torrent_sources WHERE id = ?`, sourceID); err != nil {
			t.Logf("cleanup source: %v", err)
		}
	})
	return titleID
}

// The central claim of the TV API: its watch endpoint reports the SAME locks
// resolutionLock computes, so the app renders the three gates without knowing a
// single rule behind them. If this drifts, the television and the website
// disagree about what a visitor may play — and only the website is right,
// because handlePrepareSource enforces from the same function.
func TestTVWatchLocksMatchResolutionLockIntegration(t *testing.T) {
	s := testTVServer(t)
	titleID := seedGatedTitle(t, s.store)
	userID := testUserID(t, s.store)

	session := httptest.NewRecorder()
	s.sess.setSession(session, userID)
	signedIn := session.Result().Cookies()[0]

	get := func(cookie *http.Cookie) map[string]string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/tv/v1/watch/movie/"+strconv.FormatInt(titleID, 10), nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("watch endpoint: %d %s", w.Code, w.Body)
		}
		var got struct {
			Videos []watchVideo `json:"videos"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Videos) < 3 {
			t.Fatalf("expected the three seeded sources, got %d", len(got.Videos))
		}
		locks := map[string]string{}
		for _, v := range got.Videos {
			if strings.HasPrefix(v.Name, "go-test-") {
				locks[v.Resolution] = v.Lock
				// available and lock must never disagree, or the app would show
				// a playable chip for a source the prepare endpoint refuses.
				if v.Available != (v.Lock == lockNone) {
					t.Errorf("%s: available=%v but lock=%q", v.Resolution, v.Available, v.Lock)
				}
			}
		}
		return locks
	}

	// Anonymous: 720p free, 1080p is the registration nudge, 2160p asks for
	// sign-in first because entitlements hang off an account.
	anon := get(nil)
	if anon["720p"] != lockNone || anon["1080p"] != lockMember {
		t.Errorf("anonymous locks wrong: %v", anon)
	}

	// Signed in but unentitled: 1080p opens, 4K still does not.
	member := get(signedIn)
	if member["720p"] != lockNone || member["1080p"] != lockNone {
		t.Errorf("signed-in locks wrong: %v", member)
	}
	if member["2160p"] == lockNone {
		t.Error("4K must not be playable for an unentitled account")
	}

	// Whatever the tiers evaluate to, they must equal what resolutionLock says
	// for the same access snapshot — that equality is the invariant, not the
	// specific values, which depend on whether billing is configured.
	for res, want := range map[string]string{
		"720p":  s.resolutionLock("720p", titleAccess{}),
		"1080p": s.resolutionLock("1080p", titleAccess{}),
		"2160p": s.resolutionLock("2160p", titleAccess{}),
	} {
		if anon[res] != want {
			t.Errorf("anonymous %s: API said %q, resolutionLock says %q", res, anon[res], want)
		}
	}
	for res, want := range map[string]string{
		"720p":  s.resolutionLock("720p", titleAccess{signedIn: true}),
		"1080p": s.resolutionLock("1080p", titleAccess{signedIn: true}),
		"2160p": s.resolutionLock("2160p", titleAccess{signedIn: true}),
	} {
		if member[res] != want {
			t.Errorf("signed-in %s: API said %q, resolutionLock says %q", res, member[res], want)
		}
	}
}
