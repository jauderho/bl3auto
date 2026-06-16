package bl3auto

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a Bl3Client whose endpoints all point at the given test
// server base URL.
func newTestClient(t *testing.T, baseURL string) *Bl3Client {
	t.Helper()
	hc, err := NewHttpClient()
	if err != nil {
		t.Fatalf("NewHttpClient: %v", err)
	}
	c := &Bl3Client{HttpClient: *hc}
	c.Config.BaseUrl = baseURL
	c.Config.HomeUrl = baseURL + "/home"
	c.Config.LoginUrl = baseURL + "/sessions"
	c.Config.Shift.RedemptionInfoUrl = baseURL + "/entitlement_offer_codes"
	c.Config.Shift.RedemptionUrl = baseURL + "/code_redemptions"
	c.Config.Shift.CodeListUrlV1 = baseURL + "/v1.json"
	c.Config.Shift.CodeListUrlV2 = baseURL + "/v2.json"
	return c
}

const metaTokenPage = `<html><head><meta name="csrf-token" content="test-token-12345"></head><body>ok</body></html>`

// redemptionFormHTML returns the entitlement_offer_codes partial for a code,
// offering the given services.
func redemptionFormHTML(code string, services ...string) string {
	var b strings.Builder
	b.WriteString(`<h2>Borderlands 4</h2>`)
	for _, svc := range services {
		b.WriteString(`<form class="new_archway_code_redemption" id="new_archway_code_redemption">`)
		b.WriteString(`<input type="hidden" name="authenticity_token" value="form-tok">`)
		b.WriteString(`<input type="hidden" name="archway_code_redemption[code]" value="` + code + `">`)
		b.WriteString(`<input type="hidden" name="archway_code_redemption[check]" value="chk">`)
		b.WriteString(`<input id="archway_code_redemption_service" name="archway_code_redemption[service]" value="` + svc + `">`)
		b.WriteString(`</form>`)
	}
	return b.String()
}

// --- HTTP plumbing -------------------------------------------------------

func TestHttpClientGetHeadAndDefaults(t *testing.T) {
	var gotUA, gotReferer, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		gotMethod = r.Method
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	c, err := NewHttpClient()
	if err != nil {
		t.Fatal(err)
	}
	c.SetDefaultHeader("Referer", "https://default.example/")

	// Plain Get: default headers applied.
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("default User-Agent not applied, got %q", gotUA)
	}
	if gotReferer != "https://default.example/" {
		t.Errorf("default Referer not applied, got %q", gotReferer)
	}

	// Per-request header must win over the default.
	if _, err := c.GetWithHeaders(srv.URL, map[string]string{"Referer": "https://override.example/"}); err != nil {
		t.Fatalf("GetWithHeaders: %v", err)
	}
	if gotReferer != "https://override.example/" {
		t.Errorf("per-request Referer should win, got %q", gotReferer)
	}

	if _, err := c.Head(srv.URL); err != nil {
		t.Fatalf("Head: %v", err)
	}
	if gotMethod != "HEAD" {
		t.Errorf("expected HEAD, got %q", gotMethod)
	}
}

func TestPostForm(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	c, _ := NewHttpClient()
	form := map[string][]string{"user[email]": {"a@b.com"}, "x": {"y"}}
	if _, err := c.PostForm(srv.URL, form, map[string]string{"Referer": "r"}); err != nil {
		t.Fatalf("PostForm: %v", err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", gotCT)
	}
	if !strings.Contains(gotBody, "user%5Bemail%5D=a%40b.com") || !strings.Contains(gotBody, "x=y") {
		t.Errorf("unexpected encoded body: %q", gotBody)
	}
}

func TestFetchBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	b, err := fetchBytes(srv.URL + "/good")
	if err != nil || string(b) != "payload" {
		t.Fatalf("fetchBytes good: %q, %v", b, err)
	}
	if _, err := fetchBytes(srv.URL + "/bad"); err == nil {
		t.Error("fetchBytes should error on non-200")
	}
	if _, err := fetchBytes("http://127.0.0.1:0/nope"); err == nil {
		t.Error("fetchBytes should error on bad host")
	}
}

func TestBodyAsJsonAndHtmlDoc(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			_, _ = io.WriteString(w, `{"a":1}`)
		case "/html":
			_, _ = io.WriteString(w, metaTokenPage)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, _ := NewHttpClient()

	res, _ := c.Get(srv.URL + "/json")
	jq, err := res.BodyAsJson()
	if err != nil {
		t.Fatalf("BodyAsJson: %v", err)
	}
	var v float64
	jq.From("a").Out(&v)
	if v != 1 {
		t.Errorf("expected a=1, got %v", v)
	}

	res2, _ := c.Get(srv.URL + "/html")
	doc, err := res2.BodyAsHtmlDoc()
	if err != nil {
		t.Fatalf("BodyAsHtmlDoc: %v", err)
	}
	if tok, _ := doc.Find(`meta[name="csrf-token"]`).Attr("content"); tok != "test-token-12345" {
		t.Errorf("meta token = %q", tok)
	}

	res3, _ := c.Get(srv.URL + "/missing")
	if _, err := res3.BodyAsHtmlDoc(); err == nil {
		t.Error("BodyAsHtmlDoc should error on non-200")
	}
}

// --- config --------------------------------------------------------------

func TestNewBl3ClientLocalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{
		"version": "9.9.9",
		"baseUrl": "https://shift.example",
		"requestHeaders": {"Origin": "https://o.example"},
		"shiftConfig": {"codeListUrlV2": "https://v2", "codeListUrlV1": "https://v1"}
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := NewBl3Client(path)
	if err != nil {
		t.Fatalf("NewBl3Client: %v", err)
	}
	if c.Config.Version != "9.9.9" || c.Config.BaseUrl != "https://shift.example" {
		t.Errorf("config not parsed: %+v", c.Config)
	}
	if c.headers.Get("Origin") != "https://o.example" {
		t.Errorf("request headers not applied, got %q", c.headers.Get("Origin"))
	}
}

func TestNewBl3ClientErrors(t *testing.T) {
	if _, err := NewBl3Client(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected error for missing config file")
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("not json"), 0o600)
	if _, err := NewBl3Client(bad); err == nil {
		t.Error("expected error for invalid json")
	}
}

// --- auth ----------------------------------------------------------------

func TestGetCsrfToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/meta":
			_, _ = io.WriteString(w, metaTokenPage)
		case "/input":
			_, _ = io.WriteString(w, `<form><input name="authenticity_token" value="input-tok"></form>`)
		case "/none":
			_, _ = io.WriteString(w, `<html><body>nothing here</body></html>`)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if tok, err := c.getCsrfToken(srv.URL + "/meta"); err != nil || tok != "test-token-12345" {
		t.Errorf("meta token: %q, %v", tok, err)
	}
	if tok, err := c.getCsrfToken(srv.URL + "/input"); err != nil || tok != "input-tok" {
		t.Errorf("input token: %q, %v", tok, err)
	}
	if _, err := c.getCsrfToken(srv.URL + "/none"); err == nil {
		t.Error("expected error when no token present")
	}
}

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home" {
			_, _ = io.WriteString(w, metaTokenPage)
			return
		}
		// /sessions
		_ = r.ParseForm()
		switch r.Form.Get("user[email]") {
		case "good@x.com":
			http.SetCookie(w, &http.Cookie{Name: "_session_id", Value: "sess-abc"})
			w.Header().Set("Location", "/account")
			w.WriteHeader(http.StatusFound)
		case "down@x.com":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "ok200@x.com":
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Location", "/home?redirect_to=false")
			w.WriteHeader(http.StatusFound)
		}
	}))
	defer srv.Close()

	cases := []struct {
		email   string
		wantErr bool
	}{
		{"good@x.com", false},
		{"bad@x.com", true},
		{"down@x.com", true},
		{"ok200@x.com", true},
	}
	for _, tc := range cases {
		c := newTestClient(t, srv.URL)
		c.Verbose = true // exercise logf
		err := c.Login(tc.email, "pw")
		if (err != nil) != tc.wantErr {
			t.Errorf("Login(%s): err=%v, wantErr=%v", tc.email, err, tc.wantErr)
		}
	}

	// After a successful login the session cookie should be sent on later requests.
	c := newTestClient(t, srv.URL)
	if err := c.Login("good@x.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	var sawCookie string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("_session_id"); err == nil {
			sawCookie = ck.Value
		}
	}))
	defer srv2.Close()
	// The cookie was set for srv's host; verify the jar stored it.
	if got := c.Jar.Cookies(mustParseURL(t, srv.URL)); len(got) == 0 || got[0].Value != "sess-abc" {
		t.Errorf("session cookie not stored in jar: %+v", got)
	}
	_ = sawCookie
}

// --- redemption ----------------------------------------------------------

func TestGetCodeRedemptionForms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/code_redemptions/new":
			_, _ = io.WriteString(w, metaTokenPage)
		case r.URL.Path == "/entitlement_offer_codes" && r.URL.Query().Get("code") == "GOODCODE":
			if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				t.Error("missing X-Requested-With header")
			}
			_, _ = io.WriteString(w, redemptionFormHTML("GOODCODE", "steam", "epic"))
		case r.URL.Path == "/entitlement_offer_codes" && r.URL.Query().Get("code") == "USEDCODE":
			_, _ = io.WriteString(w, `<div class="alert">This SHiFT code has already been redeemed</div>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	forms, reason, err := c.GetCodeRedemptionForms("GOODCODE")
	if err != nil || reason != "" {
		t.Fatalf("good code: reason=%q err=%v", reason, err)
	}
	if len(forms) != 2 || forms[0].Service != "steam" || forms[1].Service != "epic" {
		t.Fatalf("unexpected forms: %+v", forms)
	}
	if forms[0].Fields["archway_code_redemption[code]"] != "GOODCODE" {
		t.Errorf("form fields not parsed: %+v", forms[0].Fields)
	}

	forms, reason, err = c.GetCodeRedemptionForms("USEDCODE")
	if err != nil || len(forms) != 0 || !strings.Contains(reason, "already been redeemed") {
		t.Fatalf("used code: forms=%d reason=%q err=%v", len(forms), reason, err)
	}

	if _, _, err := c.GetCodeRedemptionForms("INVALID"); err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestRedeemFormSuccessViaPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, metaTokenPage)
		case "/code_redemptions":
			w.Header().Set("Location", "/code_redemptions/77")
			w.WriteHeader(http.StatusFound)
		case "/code_redemptions/77":
			_, _ = io.WriteString(w, `<div id="check_redemption_status" data-url="/code_redemptions/77/status"></div>`)
		case "/code_redemptions/77/status":
			_, _ = io.WriteString(w, `{"text":"Your code was successfully redeemed"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	form := RedemptionForm{Service: "steam", Fields: map[string]string{
		"authenticity_token": "x", "archway_code_redemption[code]": "C",
	}}
	if err := c.RedeemForm(form); err != nil {
		t.Fatalf("RedeemForm should succeed, got %v", err)
	}
}

func TestRedeemFormAlreadyRedeemedAlert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/code_redemptions" {
			_, _ = io.WriteString(w, `<div class="alert">This SHiFT code has already been redeemed</div>`)
			return
		}
		_, _ = io.WriteString(w, metaTokenPage)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	err := c.RedeemForm(RedemptionForm{Service: "steam", Fields: map[string]string{"a": "b"}})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "already") {
		t.Fatalf("expected already-redeemed error, got %v", err)
	}
}

// --- code lists ----------------------------------------------------------

func TestGetShiftCodesAndFailover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.json":
			_, _ = io.WriteString(w, v2Fixture)
		case "/v1.json":
			_, _ = io.WriteString(w, v1Fixture)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if v2, err := c.GetShiftCodesV2(); err != nil || len(v2) != 2 {
		t.Errorf("v2: %d codes, err=%v", len(v2), err)
	}
	if v1, err := c.GetShiftCodesV1(); err != nil || len(v1) != 2 {
		t.Errorf("v1: %d codes, err=%v", len(v1), err)
	}
	if def, err := c.GetShiftCodes(""); err != nil || len(def) != 2 {
		t.Errorf("default: %d codes, err=%v", len(def), err)
	}
	if forced, err := c.GetShiftCodes("v1"); err != nil || len(forced) != 2 {
		t.Errorf("forced v1: %d codes, err=%v", len(forced), err)
	}

	// Failover: break v2, expect v1 results.
	c.Config.Shift.CodeListUrlV2 = srv.URL + "/missing.json"
	c.Verbose = true
	fb, err := c.GetShiftCodes("")
	if err != nil || len(fb) != 2 {
		t.Errorf("failover: %d codes, err=%v", len(fb), err)
	}
}

func TestGetShiftCodesV2AllowInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, v2Fixture) // 3 entries, 1 flagged expired
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.Config.Shift.CodeListUrlV2 = srv.URL + "/v2.json"

	// Default: expired codes are dropped.
	if codes, err := c.GetShiftCodesV2(); err != nil || len(codes) != 2 {
		t.Errorf("default: expected 2 non-expired codes, got %d (err=%v)", len(codes), err)
	}

	// AllowInactive: expired codes are included.
	c.Config.Shift.AllowInactive = true
	if codes, err := c.GetShiftCodesV2(); err != nil || len(codes) != 3 {
		t.Errorf("allow-inactive: expected 3 codes incl. expired, got %d (err=%v)", len(codes), err)
	}
}

func TestRedemptionTokenCaching(t *testing.T) {
	var tokenHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code_redemptions/new":
			tokenHits++
			_, _ = io.WriteString(w, metaTokenPage)
		case "/entitlement_offer_codes":
			_, _ = io.WriteString(w, redemptionFormHTML(r.URL.Query().Get("code"), "steam"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	for _, code := range []string{"CODE1", "CODE2", "CODE3"} {
		if _, _, err := c.GetCodeRedemptionForms(code); err != nil {
			t.Fatalf("GetCodeRedemptionForms(%s): %v", code, err)
		}
	}
	if tokenHits != 1 {
		t.Errorf("expected the CSRF token endpoint to be fetched once, got %d hits", tokenHits)
	}
	if c.csrfToken != "test-token-12345" {
		t.Errorf("token not cached on client, got %q", c.csrfToken)
	}
}

// --- small helpers -------------------------------------------------------

func TestAbsoluteUrl(t *testing.T) {
	c := &Bl3Client{}
	c.Config.BaseUrl = "https://shift.example"
	cases := map[string]string{
		"/code_redemptions/1":     "https://shift.example/code_redemptions/1",
		"code_redemptions/1":      "https://shift.example/code_redemptions/1",
		"https://other.example/x": "https://other.example/x",
		"http://other.example/y":  "http://other.example/y",
	}
	for in, want := range cases {
		if got := c.absoluteUrl(in); got != want {
			t.Errorf("absoluteUrl(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusFromTextAllBranches(t *testing.T) {
	cases := []struct {
		text    string
		visited bool
		wantErr bool
	}{
		{"", true, false},
		{"", false, true},
		{"Success!", false, false},
		{"already been redeemed", false, true},
		{"this code has expired", false, true},
		{"not available", false, true},
		{"please launch a SHiFT-enabled title", false, true},
		{"some other message", false, true},
	}
	for _, tc := range cases {
		err := statusFromText(tc.text, tc.visited)
		if (err != nil) != tc.wantErr {
			t.Errorf("statusFromText(%q,%v) err=%v wantErr=%v", tc.text, tc.visited, err, tc.wantErr)
		}
	}
}

func TestStringSetAndContains(t *testing.T) {
	s := StringSet{}
	s.Add("steam")
	if _, ok := s["steam"]; !ok {
		t.Error("StringSet.Add failed")
	}

	m := ShiftCodeMap{"CODE": {"steam", "epic"}}
	if !m.Contains("CODE", "steam") {
		t.Error("Contains should be true for steam")
	}
	if m.Contains("CODE", "psn") {
		t.Error("Contains should be false for psn")
	}
	if m.Contains("MISSING", "steam") {
		t.Error("Contains should be false for missing code")
	}
}

func TestThrottlePacing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	c, _ := NewHttpClient()
	c.SetThrottle(30*time.Millisecond, 0)

	start := time.Now()
	for range 3 {
		if _, err := c.Get(srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	// First request isn't delayed; the next two are spaced ~30ms each.
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("expected pacing to add ~60ms across 3 requests, got %v", elapsed)
	}
}

func TestRateLimitedDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code_redemptions/new":
			_, _ = io.WriteString(w, metaTokenPage)
		case "/entitlement_offer_codes":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/code_redemptions":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if _, _, err := c.GetCodeRedemptionForms("X"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("entitlement 429 should report ErrRateLimited, got %v", err)
	}
	if err := c.RedeemForm(RedemptionForm{Service: "steam", Fields: map[string]string{"a": "b"}}); !errors.Is(err, ErrRateLimited) {
		t.Errorf("redemption 503 should report ErrRateLimited, got %v", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
