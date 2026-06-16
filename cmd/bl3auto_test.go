package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bl3 "github.com/jauderho/bl3auto"
	"github.com/shibukawa/configdir"
)

// shrinkBackoff sets tiny rate-limit timings (and disables pacing) for the
// duration of a test so rate-limit paths run fast.
func shrinkBackoff(t *testing.T, retries int) {
	t.Helper()
	ob, om, or := rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries
	tb, tj := throttleBase, throttleJitter
	rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = time.Millisecond, time.Millisecond, retries
	throttleBase, throttleJitter = 0, 0
	t.Cleanup(func() {
		rateLimitBaseWait, rateLimitMaxWait, rateLimitRetries = ob, om, or
		throttleBase, throttleJitter = tb, tj
	})
}

func TestRedeemedCodesRoundTrip(t *testing.T) {
	folder := &configdir.Config{Path: t.TempDir(), Type: configdir.Global}
	const fn = "test-shift-codes.json"

	// No codes/ dir at all (nil folder) → empty map.
	if got := readRedeemedCodes(nil, fn); len(got) != 0 {
		t.Errorf("nil folder should yield empty map, got %v", got)
	}
	// codes/ dir exists but has no cache file yet → empty map.
	if got := readRedeemedCodes(folder, fn); len(got) != 0 {
		t.Errorf("missing file should yield empty map, got %v", got)
	}

	// Write then read back through the codes/ folder.
	want := bl3.ShiftCodeMap{"ABCDE-FGHIJ": {"steam", "epic"}}
	if err := writeRedeemedCodes(folder, fn, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !folder.Exists(fn) {
		t.Fatalf("expected %s to exist in the codes folder after write", fn)
	}
	got := readRedeemedCodes(folder, fn)
	if !got.Contains("ABCDE-FGHIJ", "steam") || !got.Contains("ABCDE-FGHIJ", "epic") {
		t.Errorf("round-trip mismatch: %v", got)
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
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{dryrun: true}) // no single code -> full list -> bulk
	})
	if !strings.Contains(out, "Stopped early") {
		t.Errorf("expected rate-limit stop message, got:\n%s", out)
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
	c := newDoShiftClient(t, srv.URL)

	out := captureStdout(t, func() {
		doShift(c, shiftOptions{singleShiftCode: "USEDCODE", dryrun: true})
	})
	if !strings.Contains(strings.ToLower(out), "already been redeemed") {
		t.Errorf("expected already-redeemed reason, got:\n%s", out)
	}
}
