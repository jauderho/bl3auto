package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bl3 "github.com/jauderho/bl3auto"
	"github.com/shibukawa/configdir"
)

// shrinkBackoff sets tiny rate-limit timings (and disables pacing, including the
// rampup throttle/backoff) for the duration of a test so the rate-limit and rampup
// paths run fast. Rampup thresholds are saved/restored so a test may lower them after
// calling this and have the originals restored on cleanup.
func shrinkBackoff(t *testing.T, retries int) {
	t.Helper()
	ob, om, or := rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries
	tb, tj := throttleBase, throttleJitter
	rtb, rtj, rbb, rbm, rba, rsa := rampupThrottleBase, rampupThrottleJitter,
		rampupBackoffBase, rampupBackoffMax, rampupBackoffAfter, rampupStopAfter
	rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = time.Millisecond, time.Millisecond, retries
	throttleBase, throttleJitter = 0, 0
	rampupThrottleBase, rampupThrottleJitter = 0, 0
	rampupBackoffBase, rampupBackoffMax = 0, 0
	t.Cleanup(func() {
		rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = ob, om, or
		throttleBase, throttleJitter = tb, tj
		rampupThrottleBase, rampupThrottleJitter = rtb, rtj
		rampupBackoffBase, rampupBackoffMax = rbb, rbm
		rampupBackoffAfter, rampupStopAfter = rba, rsa
	})
}

// useTempCache points the redeemed-codes cache at a fresh temp dir (standing in
// for the Docker codes/ volume) for the duration of a test, and returns it.
func useTempCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := resolveCacheFolder
	resolveCacheFolder = func() *configdir.Config {
		return &configdir.Config{Path: dir, Type: configdir.Global}
	}
	t.Cleanup(func() { resolveCacheFolder = orig })
	return dir
}

// cachingTestServer serves n redeemable codes (each on steam+epic) with
// successful redemptions, plus the endpoints doShift needs.
func cachingTestServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	var b strings.Builder
	b.WriteString(`[{"meta":{},"codes":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"code":"CODE%02d","game":"Borderlands 4","expired":false}`, i)
	}
	b.WriteString(`]}]`)
	listJSON := b.String()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			code := r.URL.Query().Get("code")
			for _, svc := range []string{"steam", "epic"} {
				_, _ = io.WriteString(w, `<form class="new_archway_code_redemption" id="new_archway_code_redemption">`+
					`<input id="archway_code_redemption_service" name="archway_code_redemption[service]" value="`+svc+`">`+
					`<input name="archway_code_redemption[code]" value="`+code+`"></form>`)
			}
		case "/code_redemptions":
			_, _ = io.WriteString(w, `<div class="alert">Your code was successfully redeemed</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// TestDoShiftCachesAcrossRuns mirrors the manual two-run validation: run 1
// redeems all codes and writes the cache; run 2 reads the cache and skips them.
func TestDoShiftCachesAcrossRuns(t *testing.T) {
	shrinkBackoff(t, 5) // disable pacing so the bulk run is fast
	cacheDir := useTempCache(t)
	usernameHash = "cache-test"

	const n = 10
	srv := cachingTestServer(t, n)
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	// Run 1: a fresh cache, so every code (on both services) is redeemed.
	out1 := captureStdout(t, func() { doShift(c, shiftOptions{}) })
	if got := strings.Count(out1, "Trying "); got != n*2 {
		t.Errorf("run 1 should attempt %d redemptions, got %d:\n%s", n*2, got, out1)
	}

	// The cache must now hold all n codes (each on steam+epic).
	folder := &configdir.Config{Path: cacheDir, Type: configdir.Global}
	cached, _, _ := readRedeemedCache(folder, "cache-test-shift-codes.json")
	if len(cached) != n {
		t.Fatalf("expected %d codes cached, got %d", n, len(cached))
	}
	if !cached.Contains("CODE01", "steam") || !cached.Contains("CODE01", "epic") {
		t.Errorf("cache missing expected services: %v", cached["CODE01"])
	}

	// Run 2: everything is cached, so nothing is attempted.
	out2 := captureStdout(t, func() { doShift(c, shiftOptions{}) })
	if strings.Contains(out2, "Trying ") {
		t.Errorf("run 2 should skip all cached codes, but attempted a redemption:\n%s", out2)
	}
	if !strings.Contains(out2, "No new SHIFT codes") {
		t.Errorf("run 2 should report no new codes, got:\n%s", out2)
	}
}

func TestRedeemedCodesRoundTrip(t *testing.T) {
	folder := &configdir.Config{Path: t.TempDir(), Type: configdir.Global}
	const fn = "test-shift-codes.json"

	// No codes/ dir at all (nil folder) → empty map, no file.
	if got, _, existed := readRedeemedCache(nil, fn); len(got) != 0 || existed {
		t.Errorf("nil folder should yield empty map and existed=false, got %v existed=%v", got, existed)
	}
	// codes/ dir exists but has no cache file yet → empty map, existed=false.
	if got, _, existed := readRedeemedCache(folder, fn); len(got) != 0 || existed {
		t.Errorf("missing file should yield empty map and existed=false, got %v existed=%v", got, existed)
	}

	// Write then read back through the codes/ folder, with a stamped lastRun.
	want := bl3.ShiftCodeMap{"ABCDE-FGHIJ": {"steam", "epic"}}
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := writeRedeemedCache(folder, fn, want, stamp); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !folder.Exists(fn) {
		t.Fatalf("expected %s to exist in the codes folder after write", fn)
	}
	got, lastRun, existed := readRedeemedCache(folder, fn)
	if !existed {
		t.Error("existed should be true after write")
	}
	if !got.Contains("ABCDE-FGHIJ", "steam") || !got.Contains("ABCDE-FGHIJ", "epic") {
		t.Errorf("round-trip mismatch: %v", got)
	}
	if !lastRun.Equal(stamp) {
		t.Errorf("lastRun round-trip: got %v want %v", lastRun, stamp)
	}
}

// TestReadRedeemedCacheBackCompat: an old bare-map file (no wrapper) still reads,
// with zero lastRun and existed=true.
func TestReadRedeemedCacheBackCompat(t *testing.T) {
	folder := &configdir.Config{Path: t.TempDir(), Type: configdir.Global}
	const fn = "legacy-shift-codes.json"
	if err := folder.WriteFile(fn, []byte(`{"OLD11-OLD22":["steam","epic"]}`)); err != nil {
		t.Fatal(err)
	}
	got, lastRun, existed := readRedeemedCache(folder, fn)
	if !existed {
		t.Error("existed should be true for an old-format file")
	}
	if !got.Contains("OLD11-OLD22", "steam") || !got.Contains("OLD11-OLD22", "epic") {
		t.Errorf("old-format codes not read: %v", got)
	}
	if !lastRun.IsZero() {
		t.Errorf("old-format lastRun should be zero, got %v", lastRun)
	}
}

func TestRampupAdvised(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		existed bool
		lastRun time.Time
		want    bool
	}{
		{"no cache file", false, time.Time{}, true},
		{"old format, zero lastRun", true, time.Time{}, true},
		{"7 months ago", true, now.AddDate(0, -7, 0), true},
		{"1 month ago", true, now.AddDate(0, -1, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rampupAdvised(tc.existed, tc.lastRun, now); got != tc.want {
				t.Errorf("rampupAdvised(%v, %v) = %v, want %v", tc.existed, tc.lastRun, got, tc.want)
			}
		})
	}
}

func TestWithBackoff(t *testing.T) {
	shrinkBackoff(t, 3)

	// Bulk: rate-limited a few times, then succeeds.
	calls := 0
	err, stop := withBackoff(true, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("%w", bl3.ErrRateLimited)
		}
		return nil
	})
	if err != nil || stop || calls != 3 {
		t.Errorf("retry-then-success: err=%v stop=%v calls=%d", err, stop, calls)
	}

	// Bulk: persistent rate limiting → stop.
	if _, stop := withBackoff(true, func() error { return fmt.Errorf("%w", bl3.ErrRateLimited) }); !stop {
		t.Error("persistent rate limit should signal stop")
	}

	// Non-bulk: a rate-limit error is returned immediately, no retry, no stop.
	calls = 0
	err, stop = withBackoff(false, func() error { calls++; return fmt.Errorf("%w", bl3.ErrRateLimited) })
	if stop || !errors.Is(err, bl3.ErrRateLimited) || calls != 1 {
		t.Errorf("non-bulk: stop=%v err=%v calls=%d", stop, err, calls)
	}

	// A non-rate-limit error returns immediately without retrying.
	calls = 0
	sentinel := errors.New("boom")
	err, stop = withBackoff(true, func() error { calls++; return sentinel })
	if stop || !errors.Is(err, sentinel) || calls != 1 {
		t.Errorf("passthrough: stop=%v err=%v calls=%d", stop, err, calls)
	}
}

func TestDoShiftRateLimitStops(t *testing.T) {
	shrinkBackoff(t, 1)
	// A full-list (bulk) run: 6 codes triggers the bulk threshold so backoff
	// applies; the entitlement endpoint always rate-limits.
	bulkList := `[{"meta":{},"codes":[` +
		`{"code":"C1","expired":false},{"code":"C2","expired":false},` +
		`{"code":"C3","expired":false},{"code":"C4","expired":false},` +
		`{"code":"C5","expired":false},{"code":"C6","expired":false}]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, bulkList)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	usernameHash = "unittest-doshift"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{dryrun: true}) // no single code -> full list -> bulk
	})
	if !strings.Contains(out, "Stopped early") {
		t.Errorf("expected rate-limit stop message, got:\n%s", out)
	}
}

// rampupListJSON builds a v2 code list of n codes named CODE01..CODEnn.
func rampupListJSON(n int) string {
	var b strings.Builder
	b.WriteString(`[{"meta":{},"codes":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"code":"CODE%02d","expired":false}`, i)
	}
	b.WriteString(`]}]`)
	return b.String()
}

// TestRampupStopsAfterConsecutiveNon200: when the code-query keeps returning 302,
// rampup backs off and then stops cleanly once it hits rampupStopAfter in a row —
// without querying every remaining code.
func TestRampupStopsAfterConsecutiveNon200(t *testing.T) {
	shrinkBackoff(t, 1)
	rampupBackoffAfter, rampupStopAfter = 2, 5

	const n = 10
	listJSON := rampupListJSON(n)
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			queries++
			w.Header().Set("Location", "/home")
			w.WriteHeader(http.StatusFound) // 302: soft rate-limit
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	usernameHash = "unittest-rampup-stop"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(c, shiftOptions{rampup: true}) })

	if queries != rampupStopAfter {
		t.Errorf("expected exactly %d code queries before stopping, got %d", rampupStopAfter, queries)
	}
	if got := strings.Count(out, "Skipping "); got != rampupStopAfter {
		t.Errorf("expected %d skip lines, got %d:\n%s", rampupStopAfter, got, out)
	}
	if !strings.Contains(out, "Stopped after") {
		t.Errorf("expected shadowban stop message, got:\n%s", out)
	}
	if !strings.Contains(out, "backing off") {
		t.Errorf("expected backoff message after %d in a row, got:\n%s", rampupBackoffAfter, out)
	}
}

// TestRampupCounterResetsOn200: an interleaved 200 resets the consecutive counter,
// so a steady drip of 302s never reaches the stop threshold.
func TestRampupCounterResetsOn200(t *testing.T) {
	shrinkBackoff(t, 1)
	rampupBackoffAfter, rampupStopAfter = 100, 5 // never back off; stop at 5 in a row

	const n = 12
	listJSON := rampupListJSON(n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			// Every 3rd code answers 200 (an alert, no form) → resets the counter.
			num, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("code"), "CODE"))
			if num%3 == 0 {
				_, _ = io.WriteString(w, `<div class="alert">This SHiFT code has expired</div>`)
				return
			}
			w.Header().Set("Location", "/home")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	usernameHash = "unittest-rampup-reset"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(c, shiftOptions{rampup: true}) })

	if strings.Contains(out, "Stopped after") {
		t.Errorf("counter should reset on 200 and never stop, got:\n%s", out)
	}
	if got := strings.Count(out, "Skipping "); got != 8 { // codes not divisible by 3
		t.Errorf("expected 8 skips across %d codes, got %d:\n%s", n, got, out)
	}
}

// TestDoShiftBumpsLastRunAndUpgrades: a normal run over an old bare-map cache file
// rewrites it in the versioned format with a fresh lastRun, preserving prior codes.
func TestDoShiftBumpsLastRunAndUpgrades(t *testing.T) {
	shrinkBackoff(t, 5)
	cacheDir := useTempCache(t)
	usernameHash = "unittest-upgrade"
	fn := usernameHash + "-shift-codes.json"

	// Seed an old-format (bare map) cache file.
	folder := &configdir.Config{Path: cacheDir, Type: configdir.Global}
	if err := folder.WriteFile(fn, []byte(`{"OLDCODE-XYZ":["steam","epic"]}`)); err != nil {
		t.Fatal(err)
	}

	srv := cachingTestServer(t, 2)
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	_ = captureStdout(t, func() { doShift(c, shiftOptions{}) })

	codes, lastRun, existed := readRedeemedCache(folder, fn)
	if !existed || lastRun.IsZero() {
		t.Fatalf("expected upgraded cache with a lastRun; existed=%v lastRun=%v", existed, lastRun)
	}
	if time.Since(lastRun) > time.Minute {
		t.Errorf("lastRun should be recent, got %v", lastRun)
	}
	if !codes.Contains("OLDCODE-XYZ", "steam") {
		t.Errorf("prior codes should be preserved across upgrade: %v", codes)
	}
	if !codes.Contains("CODE01", "steam") || !codes.Contains("CODE01", "epic") {
		t.Errorf("newly redeemed codes should be recorded: %v", codes)
	}
}

func TestSummarize(t *testing.T) {
	if got := summarize("  hello   world \n"); got != "hello world" {
		t.Errorf("collapse whitespace: got %q", got)
	}
	if got := summarize("   "); got != "not redeemable" {
		t.Errorf("empty: got %q", got)
	}
	long := strings.Repeat("a", 200)
	got := summarize(long)
	if len(got) != 163 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate: len=%d suffix ok=%v", len(got), strings.HasSuffix(got, "..."))
	}
}

// captureStdout runs fn and returns everything it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// shiftTestServer emulates the SHiFT endpoints needed by doShift.
func shiftTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/code_redemptions/new":
			_, _ = io.WriteString(w, `<html><head><meta name="csrf-token" content="tok"></head></html>`)
		case r.URL.Path == "/entitlement_offer_codes" && r.URL.Query().Get("code") == "GOODCODE":
			for _, svc := range []string{"steam", "epic"} {
				_, _ = io.WriteString(w, `<form class="new_archway_code_redemption" id="new_archway_code_redemption">`+
					`<input id="archway_code_redemption_service" name="archway_code_redemption[service]" value="`+svc+`">`+
					`<input name="archway_code_redemption[code]" value="GOODCODE"></form>`)
			}
		case r.URL.Path == "/entitlement_offer_codes" && r.URL.Query().Get("code") == "USEDCODE":
			_, _ = io.WriteString(w, `<div class="alert">This SHiFT code has already been redeemed</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func newDoShiftClient(t *testing.T, baseURL string) *bl3.Bl3Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{"baseUrl":"` + baseURL + `","shiftConfig":{` +
		`"codeListUrlV2":"` + baseURL + `/v2.json",` +
		`"codeListUrlV1":"` + baseURL + `/v1.json",` +
		`"redemptionInfoUrl":"` + baseURL + `/entitlement_offer_codes",` +
		`"redemptionUrl":"` + baseURL + `/code_redemptions"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := bl3.NewBl3Client(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDoShiftSingleCodeDryRun(t *testing.T) {
	srv := shiftTestServer()
	defer srv.Close()
	usernameHash = "unittest-doshift"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{singleShiftCode: "GOODCODE", dryrun: true})
	})
	if !strings.Contains(out, "[dryrun] would redeem 'steam'") ||
		!strings.Contains(out, "[dryrun] would redeem 'epic'") {
		t.Errorf("expected dryrun output for both services, got:\n%s", out)
	}
}

func TestDoShiftPlatformFilterDryRun(t *testing.T) {
	srv := shiftTestServer()
	defer srv.Close()
	usernameHash = "unittest-doshift"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{singleShiftCode: "GOODCODE", platformFilter: []string{"steam"}, dryrun: true})
	})
	if !strings.Contains(out, "would redeem 'steam'") || strings.Contains(out, "would redeem 'epic'") {
		t.Errorf("platform filter should keep only steam, got:\n%s", out)
	}
}

func TestDoShiftAlreadyRedeemedReason(t *testing.T) {
	srv := shiftTestServer()
	defer srv.Close()
	usernameHash = "unittest-doshift"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{singleShiftCode: "USEDCODE", dryrun: true})
	})
	if !strings.Contains(strings.ToLower(out), "already been redeemed") {
		t.Errorf("expected already-redeemed reason, got:\n%s", out)
	}
}
