package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bl3 "github.com/jauderho/bl3auto"
)

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
