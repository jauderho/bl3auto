package main

import (
	"context"
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

// shrinkBackoff sets tiny rate-limit timings and disables all request pacing/backoff
// sleeps for the duration of a test so the rate-limit and bulk-backoff paths run fast.
// The consecutive-non-200 thresholds are saved/restored so a test may lower them after
// calling this and have the originals restored on cleanup.
func shrinkBackoff(t *testing.T, retries int) {
	t.Helper()
	ob, om, or := rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries
	tb, tj := throttleBase, throttleJitter
	rtb, rtj, bb, bm, ba, sa := rampupThrottleBase, rampupThrottleJitter,
		backoffBase, backoffMax, backoffAfter, stopAfter
	tc, tsf, tsp, tsa := throttleCeil, throttleSlowFactor, throttleSpeedup, throttleSpeedupAfter
	mra := maxRetryAttempts
	rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = time.Millisecond, time.Millisecond, retries
	throttleBase, throttleJitter = 0, 0
	rampupThrottleBase, rampupThrottleJitter = 0, 0
	backoffBase, backoffMax = 0, 0
	// Disable the retry queue by default so existing single-pass assertions hold; a test
	// that exercises retries opts in by setting maxRetryAttempts after calling this.
	maxRetryAttempts = 0
	t.Cleanup(func() {
		rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = ob, om, or
		throttleBase, throttleJitter = tb, tj
		rampupThrottleBase, rampupThrottleJitter = rtb, rtj
		backoffBase, backoffMax = bb, bm
		backoffAfter, stopAfter = ba, sa
		throttleCeil, throttleSlowFactor, throttleSpeedup, throttleSpeedupAfter = tc, tsf, tsp, tsa
		maxRetryAttempts = mra
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
	out1 := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
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
	out2 := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if strings.Contains(out2, "Trying ") {
		t.Errorf("run 2 should skip all cached codes, but attempted a redemption:\n%s", out2)
	}
	if !strings.Contains(out2, "No new SHIFT codes") {
		t.Errorf("run 2 should report no new codes, got:\n%s", out2)
	}
}

func TestSleepCtxInterrupted(t *testing.T) {
	// A live context: a short sleep completes and returns true.
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx with a live context should return true")
	}
	// A cancelled context: returns false promptly without waiting out d.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx with a cancelled context should return false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleepCtx should abort promptly on cancel, took %s", elapsed)
	}
	// Zero/negative duration reflects the live/cancelled context state.
	if !sleepCtx(context.Background(), 0) {
		t.Error("sleepCtx(ctx,0) on a live context should return true")
	}
	if sleepCtx(ctx, 0) {
		t.Error("sleepCtx(ctx,0) on a cancelled context should return false")
	}
}

// TestDoShiftInterruptSavesProgress: a Ctrl-C (context cancel) mid-run stops
// cleanly between codes and the cache holds what was redeemed so far.
func TestDoShiftInterruptSavesProgress(t *testing.T) {
	shrinkBackoff(t, 5)
	usernameHash = "unittest-interrupt"
	useTempCache(t)

	ctx, cancel := context.WithCancel(context.Background())
	listJSON := rampupListJSON(5)
	queries := 0
	redemptions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			queries++
			code := r.URL.Query().Get("code")
			for _, svc := range []string{"steam", "epic"} {
				_, _ = io.WriteString(w, `<form class="new_archway_code_redemption" id="new_archway_code_redemption">`+
					`<input id="archway_code_redemption_service" name="archway_code_redemption[service]" value="`+svc+`">`+
					`<input name="archway_code_redemption[code]" value="`+code+`"></form>`)
			}
		case "/code_redemptions":
			redemptions++
			if redemptions == 2 {
				// Simulate Ctrl-C once the first redemption (steam) has landed,
				// just before the second (epic) would be answered — so the
				// steam success isn't racing its own response body read.
				cancel()
			}
			_, _ = io.WriteString(w, `<div class="alert">Your code was successfully redeemed</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(ctx, c, shiftOptions{}) })
	if !strings.Contains(out, "Interrupted") {
		t.Errorf("expected the interrupted message, got:\n%s", out)
	}
	// Only the first code was queried before the interrupt was observed at the
	// top of the next iteration.
	if queries != 1 {
		t.Errorf("expected the run to stop after the first code, got %d queries", queries)
	}
	cached, _, _ := readRedeemedCache(resolveCacheFolder(), usernameHash+"-shift-codes.json")
	if !cached.Contains("CODE01", "steam") {
		t.Errorf("the first code's progress should be saved, got %v", cached)
	}
	if _, ok := cached["CODE02"]; ok {
		t.Errorf("the second code should not have been processed: %v", cached)
	}
}

// TestAtomicWriteCacheReplacesCleanly verifies the cache write is atomic: it overwrites
// an existing file via rename and leaves no temp files behind, so a crash mid-write
// (kill -9, full disk, power loss) can never truncate or corrupt the cache.
func TestAtomicWriteCacheReplacesCleanly(t *testing.T) {
	dir := t.TempDir()
	folder := &configdir.Config{Path: dir, Type: configdir.Global}
	const fn = "atomic-shift-codes.json"

	// Seed an existing cache, then overwrite it with different contents.
	if err := writeRedeemedCache(folder, fn, bl3.ShiftCodeMap{"OLD": {"steam"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := writeRedeemedCache(folder, fn, bl3.ShiftCodeMap{"NEW": {"epic"}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The directory must hold exactly the destination file — no leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != fn {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only %q in cache dir, got %v", fn, names)
	}

	// The latest write must be readable and complete (valid JSON, new content only).
	codes, _, existed := readRedeemedCache(folder, fn)
	if !existed || !codes.Contains("NEW", "epic") || codes.Contains("OLD", "steam") {
		t.Errorf("expected the latest cache content, got existed=%v codes=%v", existed, codes)
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
	err, stop := withBackoff(t.Context(), true, func() error {
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
	if _, stop := withBackoff(t.Context(), true, func() error { return fmt.Errorf("%w", bl3.ErrRateLimited) }); !stop {
		t.Error("persistent rate limit should signal stop")
	}

	// Non-bulk: a rate-limit error is returned immediately, no retry, no stop.
	calls = 0
	err, stop = withBackoff(t.Context(), false, func() error { calls++; return fmt.Errorf("%w", bl3.ErrRateLimited) })
	if stop || !errors.Is(err, bl3.ErrRateLimited) || calls != 1 {
		t.Errorf("non-bulk: stop=%v err=%v calls=%d", stop, err, calls)
	}

	// A non-rate-limit error returns immediately without retrying.
	calls = 0
	sentinel := errors.New("boom")
	err, stop = withBackoff(t.Context(), true, func() error { calls++; return sentinel })
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
		doShift(context.Background(), c, shiftOptions{dryrun: true}) // no single code -> full list -> bulk
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
// rampup backs off and then stops cleanly once it hits stopAfter in a row —
// without querying every remaining code.
func TestRampupStopsAfterConsecutiveNon200(t *testing.T) {
	shrinkBackoff(t, 1)
	backoffAfter, stopAfter = 2, 5

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

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{rampup: true}) })

	if queries != stopAfter {
		t.Errorf("expected exactly %d code queries before stopping, got %d", stopAfter, queries)
	}
	if got := strings.Count(out, "Skipping "); got != stopAfter {
		t.Errorf("expected %d skip lines, got %d:\n%s", stopAfter, got, out)
	}
	if !strings.Contains(out, "Stopped after") {
		t.Errorf("expected shadowban stop message, got:\n%s", out)
	}
	if !strings.Contains(out, "backing off") {
		t.Errorf("expected backoff message after %d in a row, got:\n%s", backoffAfter, out)
	}
}

// TestRampupCounterResetsOn200: an interleaved 200 resets the consecutive counter,
// so a steady drip of 302s never reaches the stop threshold.
func TestRampupCounterResetsOn200(t *testing.T) {
	shrinkBackoff(t, 1)
	backoffAfter, stopAfter = 100, 5 // never back off; stop at 5 in a row

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

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{rampup: true}) })

	if strings.Contains(out, "Stopped after") {
		t.Errorf("counter should reset on 200 and never stop, got:\n%s", out)
	}
	if got := strings.Count(out, "Skipping "); got != 8 { // codes not divisible by 3
		t.Errorf("expected 8 skips across %d codes, got %d:\n%s", n, got, out)
	}
}

// TestDoShiftRetriesThrottledCodesAtEndOfRun: a code that 302s on its first query is
// re-queued and retried at the end of the run, where a now-200 response lets it redeem.
func TestDoShiftRetriesThrottledCodesAtEndOfRun(t *testing.T) {
	shrinkBackoff(t, 1)
	maxRetryAttempts = 5
	backoffAfter, stopAfter = 100, 100 // don't back off or stop during this test

	const n = 3
	listJSON := rampupListJSON(n)
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			code := r.URL.Query().Get("code")
			seen[code]++
			if seen[code] == 1 {
				// First query for this code: throttle with a 302.
				w.Header().Set("Location", "/home")
				w.WriteHeader(http.StatusFound)
				return
			}
			// Retry: hand back a redeemable steam form.
			_, _ = io.WriteString(w, `<form class="new_archway_code_redemption" id="new_archway_code_redemption">`+
				`<input id="archway_code_redemption_service" name="archway_code_redemption[service]" value="steam">`+
				`<input name="archway_code_redemption[code]" value="`+code+`"></form>`)
		case "/code_redemptions":
			_, _ = io.WriteString(w, `<div class="alert">Your code was successfully redeemed</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	usernameHash = "unittest-retry"
	cacheDir := useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{rampup: true}) })

	if !strings.Contains(out, "will retry") {
		t.Errorf("expected a retry notice, got:\n%s", out)
	}
	if strings.Contains(out, "Stopped after") {
		t.Errorf("run should not hit the shadowban stop, got:\n%s", out)
	}
	folder := &configdir.Config{Path: cacheDir, Type: configdir.Global}
	cached, _, _ := readRedeemedCache(folder, "unittest-retry-shift-codes.json")
	for i := 1; i <= n; i++ {
		code := fmt.Sprintf("CODE%02d", i)
		if !cached.Contains(code, "steam") {
			t.Errorf("expected %s redeemed on steam after retry; cache=%v", code, cached)
		}
		if seen[code] != 2 {
			t.Errorf("expected %s queried twice (302 then 200), got %d", code, seen[code])
		}
	}
}

// TestDoShiftGivesUpAfterMaxRetries: a code that 302s forever is queried exactly
// 1+maxRetryAttempts times, then dropped (no infinite loop), with a closing note.
func TestDoShiftGivesUpAfterMaxRetries(t *testing.T) {
	shrinkBackoff(t, 1)
	maxRetryAttempts = 3
	backoffAfter, stopAfter = 100, 100 // never back off or stop on the consecutive count

	listJSON := rampupListJSON(1)
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
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	usernameHash = "unittest-giveup"
	useTempCache(t)
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{rampup: true}) })

	if want := 1 + maxRetryAttempts; queries != want {
		t.Errorf("expected %d queries (initial + retries), got %d", want, queries)
	}
	if !strings.Contains(out, "still throttled after") {
		t.Errorf("expected give-up note, got:\n%s", out)
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

	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })

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

// TestMigrateInPlace: --migrate upgrades an old bare-map file to the versioned
// format in place (preserving codes and the zero lastRun), and is idempotent.
func TestMigrateInPlace(t *testing.T) {
	cacheDir := useTempCache(t)
	usernameHash = "unittest-migrate"
	fn := usernameHash + "-shift-codes.json"
	folder := &configdir.Config{Path: cacheDir, Type: configdir.Global}

	if err := folder.WriteFile(fn, []byte(`{"OLD11-OLD22":["steam","epic"],"OLD33-OLD44":["steam"]}`)); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, runMigrate)
	if !strings.Contains(out, "Migrated 2 codes to cache version 2") {
		t.Errorf("expected migrate summary, got:\n%s", out)
	}

	// File is now the versioned format, codes preserved, lastRun still zero.
	data, err := folder.ReadFile(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 2`) {
		t.Errorf("migrated file should carry version 2:\n%s", data)
	}
	codes, lastRun, existed := readRedeemedCache(folder, fn)
	if !existed || !codes.Contains("OLD11-OLD22", "epic") || !codes.Contains("OLD33-OLD44", "steam") {
		t.Errorf("codes not preserved across migrate: %v", codes)
	}
	if !lastRun.IsZero() {
		t.Errorf("migrate should preserve the unknown (zero) lastRun, got %v", lastRun)
	}

	// Idempotent: a second migrate is a no-op.
	out = captureStdout(t, runMigrate)
	if !strings.Contains(out, "already version 2") {
		t.Errorf("second migrate should be a no-op, got:\n%s", out)
	}
}

// TestMigrateNoCache: --migrate with no cache file reports nothing to do.
func TestMigrateNoCache(t *testing.T) {
	useTempCache(t)
	usernameHash = "unittest-migrate-empty"
	out := captureStdout(t, runMigrate)
	if !strings.Contains(out, "No redeemed-codes cache to migrate") {
		t.Errorf("expected no-cache message, got:\n%s", out)
	}
}

// TestDoShiftCountLimit: --count stops the run after N successful redemptions.
func TestDoShiftCountLimit(t *testing.T) {
	shrinkBackoff(t, 5)
	usernameHash = "unittest-count"
	useTempCache(t)
	srv := cachingTestServer(t, 5) // 5 codes x steam+epic = up to 10 successes
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{count: 2}) })

	if got := strings.Count(out, "Trying "); got != 2 {
		t.Errorf("--count 2 should attempt exactly 2 redemptions, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "Reached the --count limit of 2") {
		t.Errorf("expected --count limit message, got:\n%s", out)
	}
}

// TestDoShiftExpiredCachedAndSkipped: an expired code is recorded in the cache and
// not re-queried on the next run.
func TestDoShiftExpiredCachedAndSkipped(t *testing.T) {
	shrinkBackoff(t, 5)
	usernameHash = "unittest-expired"
	useTempCache(t)

	queries := 0
	listJSON := `[{"meta":{},"codes":[{"code":"EXPCODE","expired":false}]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			queries++
			_, _ = io.WriteString(w, `<div class="alert">This SHiFT code has expired</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	// Run 1: the code is queried once, found expired, and cached as such.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if queries != 1 {
		t.Fatalf("run 1 should query the code once, got %d", queries)
	}
	folder := resolveCacheFolder()
	cached, _, _ := readRedeemedCache(folder, usernameHash+"-shift-codes.json")
	if !cached.Contains("EXPCODE", expiredMarker) {
		t.Fatalf("expired code should be cached with the expired marker: %v", cached)
	}

	// Run 2: the expired code is skipped outright — no further query.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if queries != 1 {
		t.Errorf("run 2 should skip the expired code (no new query), got %d total queries", queries)
	}
}

// TestResolveCacheFolderPrefersLocal: the real resolveCacheFolder uses a local
// codes/ dir when present, and falls back to the config dir otherwise.
func TestResolveCacheFolderPrefersLocal(t *testing.T) {
	// t.TempDir registers its own RemoveAll cleanup; create it first so the
	// chdir-back cleanup below is registered after it and therefore runs *before*
	// it (cleanups are LIFO). On Windows RemoveAll fails on a directory that is
	// still a process's current working directory, so we must chdir out first.
	dir := t.TempDir()

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// No codes/ dir → config-dir fallback (not the local path).
	if f := resolveCacheFolder(); f.Type == configdir.Local && f.Path == "codes" {
		t.Errorf("without a codes/ dir, should not use the local path; got %+v", f)
	}

	// With a codes/ dir → local path.
	if err := os.Mkdir("codes", 0o755); err != nil {
		t.Fatal(err)
	}
	if f := resolveCacheFolder(); f.Path != "codes" || f.Type != configdir.Local {
		t.Errorf("with a codes/ dir, expected local codes path, got %+v", f)
	}
}

// countingShiftServer serves the given v2 list and offers steam+epic forms with
// successful redemptions, counting code-query requests via the returned pointer.
func countingShiftServer(t *testing.T, listJSON string) (*httptest.Server, *int) {
	t.Helper()
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			queries++
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
	return srv, &queries
}

// TestDoShiftMarksCompleteAndSkipsQuery: once a code is redeemed on every offered
// platform it is marked complete and skipped (no query) next run; --refresh forces
// a re-query.
func TestDoShiftMarksCompleteAndSkipsQuery(t *testing.T) {
	shrinkBackoff(t, 5)
	usernameHash = "unittest-complete"
	useTempCache(t)
	srv, queries := countingShiftServer(t, `[{"meta":{},"codes":[{"code":"ALLCODE","expired":false}]}]`)
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	// Run 1: query once, redeem steam+epic → marked complete.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if *queries != 1 {
		t.Fatalf("run 1 should query once, got %d", *queries)
	}
	cached, _, _ := readRedeemedCache(resolveCacheFolder(), usernameHash+"-shift-codes.json")
	if !cached.Contains("ALLCODE", completeMarker) {
		t.Fatalf("code redeemed on all platforms should be marked complete: %v", cached)
	}

	// Run 2 (no --refresh): the complete code is skipped without a query.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if *queries != 1 {
		t.Errorf("run 2 should skip the complete code, got %d total queries", *queries)
	}

	// Run 3 (--refresh): the complete code is re-queried.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{refresh: true}) })
	if *queries != 2 {
		t.Errorf("--refresh should re-query the complete code, got %d total queries", *queries)
	}
}

// TestPlatformFilterDoesNotComplete: a --platform run leaves other platforms
// un-redeemed, so the code is not marked complete and is re-queried next run.
func TestPlatformFilterDoesNotComplete(t *testing.T) {
	shrinkBackoff(t, 5)
	usernameHash = "unittest-filter-complete"
	useTempCache(t)
	srv, queries := countingShiftServer(t, `[{"meta":{},"codes":[{"code":"ALLCODE","expired":false}]}]`)
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{platformFilter: []string{"steam"}}) })
	cached, _, _ := readRedeemedCache(resolveCacheFolder(), usernameHash+"-shift-codes.json")
	if cached.Contains("ALLCODE", completeMarker) {
		t.Errorf("a platform-filtered run must not mark a code complete: %v", cached)
	}
	if !cached.Contains("ALLCODE", "steam") || cached.Contains("ALLCODE", "epic") {
		t.Errorf("only steam should be redeemed under the filter: %v", cached)
	}

	// Next run (no filter) must re-query, since epic is still pending.
	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if *queries != 2 {
		t.Errorf("incomplete code should be re-queried, got %d total queries", *queries)
	}
}

// TestBulkStopsOn302WithoutRampup: the dialed-back default — even without --rampup,
// a bulk run stops after stopAfter consecutive non-200 responses.
func TestBulkStopsOn302WithoutRampup(t *testing.T) {
	shrinkBackoff(t, 1)
	backoffAfter, stopAfter = 3, 6
	usernameHash = "unittest-bulkstop"
	useTempCache(t)

	listJSON := rampupListJSON(20)
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
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) }) // 20 codes => bulk, no --rampup
	if queries != stopAfter {
		t.Errorf("bulk run should stop after %d consecutive non-200s, got %d queries", stopAfter, queries)
	}
	if !strings.Contains(out, "Stopped after") {
		t.Errorf("expected the stop message without --rampup, got:\n%s", out)
	}
}

// TestDoShiftSlowsOnRepeated302: the adaptive throttle widens the request spacing
// on repeated non-200 code queries, up to the ceiling.
func TestDoShiftSlowsOnRepeated302(t *testing.T) {
	shrinkBackoff(t, 1)
	backoffAfter, stopAfter = 100, 100 // keep the emergency brake from firing/stopping
	throttleBase, throttleJitter = time.Millisecond, 0
	throttleCeil = 8 * time.Millisecond
	throttleSlowFactor = 2.0
	usernameHash = "unittest-aimd-slow"
	useTempCache(t)

	listJSON := rampupListJSON(10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			w.WriteHeader(http.StatusFound) // 302 throttle (no Location → not an auth redirect)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) }) // 10 codes => bulk
	if got := c.CurrentInterval(); got <= time.Millisecond {
		t.Errorf("repeated 302s should widen the throttle above the 1ms floor, got %s", got)
	}
	if got := c.CurrentInterval(); got > throttleCeil {
		t.Errorf("throttle should be capped at ceil %s, got %s", throttleCeil, got)
	}
}

// TestDoShiftSpeedsUpOnCleanStreak: an all-clean bulk run never drifts the throttle
// above the configured floor (and exercises the speed-up path).
func TestDoShiftSpeedsUpOnCleanStreak(t *testing.T) {
	shrinkBackoff(t, 1)
	throttleBase, throttleJitter = 2*time.Millisecond, 0
	throttleSpeedup = time.Millisecond
	throttleSpeedupAfter = 3
	usernameHash = "unittest-aimd-fast"
	useTempCache(t)

	srv := cachingTestServer(t, 10) // 10 codes, all redeem cleanly
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if got := c.CurrentInterval(); got != 2*time.Millisecond {
		t.Errorf("a clean run should hold the throttle at the 2ms floor, got %s", got)
	}
}

func TestIsAuthRedirect(t *testing.T) {
	cases := map[string]bool{
		"":                         false,
		"/home":                    false, // SHiFT's throttle target, NOT a sign-in bounce
		"/entitlement_offer_codes": false,
		"/login":                   true,
		"/sessions/new":            true,
		"/home?redirect_to=false":  true,
	}
	for loc, want := range cases {
		if got := isAuthRedirect(loc); got != want {
			t.Errorf("isAuthRedirect(%q) = %v, want %v", loc, got, want)
		}
	}
}

// TestWithBackoffHonoursRetryAfter: a server-specified Retry-After wins over the
// exponential backoff, so withBackoff waits the short server time, not the long
// exponential one.
func TestWithBackoffHonoursRetryAfter(t *testing.T) {
	ob, om, or := rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries
	rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = time.Hour, time.Hour, 3
	t.Cleanup(func() { rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = ob, om, or })

	calls := 0
	start := time.Now()
	err, stop := withBackoff(t.Context(), true, func() error {
		calls++
		if calls == 1 {
			return &bl3.RateLimitError{Status: 429, RetryAfter: 15 * time.Millisecond}
		}
		return nil
	})
	if err != nil || stop {
		t.Fatalf("expected success after honoring Retry-After, got err=%v stop=%v", err, stop)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("should have slept ~15ms (Retry-After), not the 1h exponential; took %s", elapsed)
	}
	if calls != 2 {
		t.Errorf("expected 2 op calls, got %d", calls)
	}
}

// TestDoShiftStopsOnSessionExpiry: a query 302 that redirects to sign in is treated
// as a lost session (not a throttle) — the run stops immediately with a clear message
// rather than counting toward the shadowban brake.
func TestDoShiftStopsOnSessionExpiry(t *testing.T) {
	shrinkBackoff(t, 1)
	usernameHash = "unittest-session"
	useTempCache(t)

	listJSON := rampupListJSON(10)
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, listJSON)
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, `<meta name="csrf-token" content="t">`)
		case "/entitlement_offer_codes":
			queries++
			w.Header().Set("Location", "/sessions/new") // bounced to sign in
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{}) })
	if queries != 1 {
		t.Errorf("session-expiry should stop on the first redirect, got %d queries", queries)
	}
	if !strings.Contains(out, "session expired") {
		t.Errorf("expected the session-expiry message, got:\n%s", out)
	}
}

// TestGameFilter: --skip-game / --game filter the candidate list before querying, so
// codes for excluded games are never queried.
func TestGameFilter(t *testing.T) {
	listJSON := `[{"meta":{},"codes":[` +
		`{"code":"BL3CODE","game":"Borderlands 3","expired":false},` +
		`{"code":"BL4CODE","game":"Borderlands 4","expired":false}]}]`

	t.Run("skip", func(t *testing.T) {
		shrinkBackoff(t, 5)
		usernameHash = "unittest-skipgame"
		useTempCache(t)
		srv, queries := countingShiftServer(t, listJSON)
		defer srv.Close()
		c := newDoShiftClient(t, srv.URL)
		_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{gameSkip: []string{"borderlands 4"}}) })
		if *queries != 1 {
			t.Errorf("skip-game should query only the non-skipped code, got %d", *queries)
		}
		cached, _, _ := readRedeemedCache(resolveCacheFolder(), usernameHash+"-shift-codes.json")
		if cached.Contains("BL4CODE", "steam") {
			t.Errorf("a skipped game's code must not be redeemed: %v", cached)
		}
	})

	t.Run("include", func(t *testing.T) {
		shrinkBackoff(t, 5)
		usernameHash = "unittest-incgame"
		useTempCache(t)
		srv, queries := countingShiftServer(t, listJSON)
		defer srv.Close()
		c := newDoShiftClient(t, srv.URL)
		_ = captureStdout(t, func() { doShift(context.Background(), c, shiftOptions{gameInclude: []string{"borderlands 3"}}) })
		if *queries != 1 {
			t.Errorf("include-game should query only the matching code, got %d", *queries)
		}
	})
}

func TestMatchesGame(t *testing.T) {
	cases := []struct {
		name          string
		include, skip []string
		game          string
		want          bool
	}{
		{"no filters", nil, nil, "Borderlands 4", true},
		{"skip matches", nil, []string{"borderlands 4"}, "Borderlands 4", false},
		{"skip misses", nil, []string{"borderlands 4"}, "Borderlands 3", true},
		{"include matches", []string{"wonderlands"}, nil, "Tiny Tina's Wonderlands", true},
		{"include misses", []string{"borderlands 3"}, nil, "Borderlands 4", false},
		{"skip wins over include", []string{"borderlands"}, []string{"borderlands 4"}, "Borderlands 4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesGame(tc.include, tc.skip, tc.game); got != tc.want {
				t.Errorf("matchesGame(%v,%v,%q)=%v want %v", tc.include, tc.skip, tc.game, got, tc.want)
			}
		})
	}
}

func TestAccountPlatforms(t *testing.T) {
	m := bl3.ShiftCodeMap{
		"A": {"epic", "steam"},
		"B": {"steam", completeMarker},
		"C": {expiredMarker},
	}
	got := accountPlatforms(m)
	if len(got) != 2 || got[0] != "epic" || got[1] != "steam" {
		t.Errorf("accountPlatforms should return sorted real services {epic,steam}, got %v", got)
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
	c, err := bl3.NewBl3Client(t.Context(), path)
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
		doShift(context.Background(), c, shiftOptions{singleShiftCode: "GOODCODE", dryrun: true})
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
		doShift(context.Background(), c, shiftOptions{singleShiftCode: "GOODCODE", platformFilter: []string{"steam"}, dryrun: true})
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
		doShift(context.Background(), c, shiftOptions{singleShiftCode: "USEDCODE", dryrun: true})
	})
	if !strings.Contains(strings.ToLower(out), "already been redeemed") {
		t.Errorf("expected already-redeemed reason, got:\n%s", out)
	}
}
