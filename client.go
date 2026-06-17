package bl3auto

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ErrRateLimited is returned when SHiFT responds with 429 (Too Many Requests)
// or 503 (Service Unavailable). Callers should back off rather than retry hard.
var ErrRateLimited = errors.New("rate limited by SHiFT")

// RateLimitError carries a 429/503 rate-limit response, including any Retry-After
// the server specified so callers can honour it instead of guessing a backoff. It
// satisfies errors.Is(err, ErrRateLimited), so existing sentinel checks keep working.
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited by SHiFT (status %d)", e.Status)
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// CodeQueryStatusError is returned by GetCodeRedemptionForms when the code-query GET
// answers with a non-200, non-rate-limit status (commonly a 302 redirect — SHiFT's
// soft rate-limit / shadowban signal). Location is the redirect target (used to tell
// a lost session apart from a throttle) and RetryAfter any server-specified wait.
type CodeQueryStatusError struct {
	Status     int
	Location   string
	RetryAfter time.Duration
}

func (e *CodeQueryStatusError) Error() string {
	return fmt.Sprintf("code query returned status %d", e.Status)
}

// parseRetryAfter reads a Retry-After header (delta-seconds or HTTP-date) into a
// duration. It returns 0 when the header is absent, malformed, or already in the past.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// remoteConfigUrl is the location of the published runtime config. It is fetched
// at startup so the GearBox endpoints can be hot-fixed without a release. It is a
// var so tests can point it elsewhere.
var remoteConfigUrl = "https://raw.githubusercontent.com/jauderho/bl3auto/main/config.json"

// embeddedConfig is the config.json compiled into the binary. It is the fallback
// used when the remote config is unreachable or incompatible with this build, so
// a freshly compiled binary always works without --config or network access.
//
//go:embed config.json
var embeddedConfig []byte

type HttpClient struct {
	http.Client
	headers http.Header

	// Request pacing (see SetThrottle). Zero minInterval disables pacing.
	// minInterval adapts at runtime (see Slowdown/Speedup); minIntervalFloor is
	// the configured base it never drops below.
	minInterval      time.Duration
	minIntervalFloor time.Duration
	jitter           time.Duration
	lastRequest      time.Time
}

type HttpResponse struct {
	http.Response
}

func NewHttpClient() (*HttpClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, errors.New("failed to setup cookies")
	}

	return &HttpClient{
		Client: http.Client{
			Jar: jar,
			// Don't auto-follow redirects: the SHiFT login (302) and code
			// redemption (302) flows need to inspect the Location header and
			// drive the redirect chain manually, mirroring autoshift.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		headers: http.Header{
			// Browser-like headers; the SHiFT site rejects the old "BL3 Auto SHiFT"
			// User-Agent. Accept-Encoding is intentionally left unset so Go's
			// transport negotiates and transparently decompresses gzip.
			"User-Agent":      []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0"},
			"Accept":          []string{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			"Accept-Language": []string{"en-US,en;q=0.5"},
		},
	}, nil
}

func (response *HttpResponse) BodyAsHtmlDoc() (*goquery.Document, error) {
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != 200 {
		return nil, errors.New("invalid response code")
	}

	doc, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, errors.New("invalid html")
	}

	return doc, nil
}

func getResponse(res *http.Response, err error) (*HttpResponse, error) {
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("received nil response")
	}
	return &HttpResponse{
		*res,
	}, nil
}

func (client *HttpClient) SetDefaultHeader(k, v string) {
	client.headers.Set(k, v)
}

// SetThrottle paces outgoing requests so consecutive calls are spaced at least
// minInterval apart, plus a random amount up to jitter. This keeps bulk
// redemption under SHiFT's rate limits and makes traffic look less bot-like.
// minInterval also becomes the floor that adaptive speed-ups never drop below.
func (client *HttpClient) SetThrottle(minInterval, jitter time.Duration) {
	client.minInterval = minInterval
	client.minIntervalFloor = minInterval
	client.jitter = jitter
}

// CurrentInterval reports the current minimum request spacing, which adapts at
// runtime via Slowdown/Speedup.
func (client *HttpClient) CurrentInterval() time.Duration {
	return client.minInterval
}

// Slowdown widens the request spacing multiplicatively (on a rate-limit signal),
// capped at ceil. The jitter is unchanged. A no-op when pacing is disabled.
func (client *HttpClient) Slowdown(factor float64, ceil time.Duration) {
	if client.minInterval <= 0 || factor <= 1 {
		return
	}
	next := time.Duration(float64(client.minInterval) * factor)
	if next > ceil {
		next = ceil
	}
	client.minInterval = next
}

// Speedup narrows the request spacing additively (after a clean streak), never
// below the floor set by SetThrottle. A no-op when pacing is disabled.
func (client *HttpClient) Speedup(step time.Duration) {
	if client.minInterval <= 0 || step <= 0 {
		return
	}
	if next := client.minInterval - step; next > client.minIntervalFloor {
		client.minInterval = next
	} else {
		client.minInterval = client.minIntervalFloor
	}
}

// pace sleeps as needed to honour the configured request spacing.
func (client *HttpClient) pace() {
	if client.minInterval <= 0 {
		return
	}
	target := client.minInterval
	if client.jitter > 0 {
		target += time.Duration(rand.Int64N(int64(client.jitter) + 1))
	}
	if d := time.Until(client.lastRequest.Add(target)); d > 0 {
		time.Sleep(d)
	}
	client.lastRequest = time.Now()
}

func (client *HttpClient) Do(req *http.Request) (*HttpResponse, error) {
	client.pace()
	// Default headers only fill in what the caller hasn't set, so per-request
	// headers (Referer, X-CSRF-Token, X-Requested-With, ...) are preserved.
	for k, v := range client.headers {
		if req.Header.Get(k) != "" {
			continue
		}
		for _, x := range v {
			req.Header.Set(k, x)
		}
	}
	return getResponse(client.Client.Do(req))
}

func (client *HttpClient) Get(rawurl string) (*HttpResponse, error) {
	return client.GetWithHeaders(rawurl, nil)
}

func (client *HttpClient) GetWithHeaders(rawurl string, headers map[string]string) (*HttpResponse, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// PostForm submits url-encoded form data with optional per-request headers.
func (client *HttpClient) PostForm(rawurl string, data url.Values, headers map[string]string) (*HttpResponse, error) {
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// fetchBytes retrieves a public URL using the default (redirect-following) HTTP
// client. Used for the remote config and SHiFT code lists hosted on GitHub raw,
// which may 302 to a CDN and so cannot use the no-redirect SHiFT client.
//
// Accept-Encoding is deliberately left unset: Go's transport then adds
// "Accept-Encoding: gzip" automatically and transparently decompresses the
// response, so the ~234 KB code list is fetched gzip-compressed for free.
// Setting the header manually would disable that automatic decompression.
func fetchBytes(rawurl string) ([]byte, error) {
	resp, err := http.Get(rawurl) //nolint:gosec // URLs come from trusted config
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned status %d", rawurl, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

type Bl3Client struct {
	HttpClient
	Config    Bl3Config
	Verbose   bool
	csrfToken string // cached session CSRF token for redemption requests
}

// NewBl3Client builds a client from config. When configPath is non-empty the
// config is read from that local file (useful for testing new URLs before they
// are merged); otherwise it is fetched from the published remote config.
func NewBl3Client(configPath string) (*Bl3Client, error) {
	client, err := NewHttpClient()
	if err != nil {
		return nil, errors.New("failed to start client")
	}

	config, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	for header, value := range config.RequestHeaders {
		client.SetDefaultHeader(header, value)
	}

	return &Bl3Client{
		HttpClient: *client,
		Config:     config,
	}, nil
}

// configComplete reports whether a parsed config carries the endpoints the SHiFT
// login/redemption flow needs. Used to reject a stale or incompatible remote
// config (e.g. the pre-2.3.0 schema, which lacks these fields).
func configComplete(c Bl3Config) bool {
	return c.BaseUrl != "" && c.HomeUrl != "" && c.LoginUrl != "" &&
		c.Shift.RedemptionInfoUrl != "" && c.Shift.RedemptionUrl != "" &&
		c.Shift.CodeListUrlV2 != ""
}

// loadConfig resolves the runtime config. An explicit --config path is used
// as-is. Otherwise the published remote config is tried first (so endpoints can
// be hot-fixed without a release); if it is unreachable, unparseable, or
// incompatible with this binary, the embedded config.json is used instead.
func loadConfig(configPath string) (Bl3Config, error) {
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return Bl3Config{}, errors.New("failed to read config '" + configPath + "': " + err.Error())
		}
		config := Bl3Config{}
		if err := json.Unmarshal(data, &config); err != nil {
			return Bl3Config{}, errors.New("failed to parse config '" + configPath + "': " + err.Error())
		}
		return config, nil
	}

	if data, err := fetchBytes(remoteConfigUrl); err == nil {
		remote := Bl3Config{}
		if json.Unmarshal(data, &remote) == nil && configComplete(remote) {
			return remote, nil
		}
	}

	// Remote was missing or incompatible; use the config baked into the binary.
	config := Bl3Config{}
	if err := json.Unmarshal(embeddedConfig, &config); err != nil {
		return Bl3Config{}, errors.New("failed to parse embedded config: " + err.Error())
	}
	return config, nil
}

func (client *Bl3Client) logf(format string, args ...any) {
	if client.Verbose {
		_, _ = fmt.Fprintf(os.Stderr, "[verbose] "+format+"\n", args...)
	}
}

// getCsrfToken fetches a SHiFT page and extracts its CSRF token. SHiFT exposes
// the token in a <meta name="csrf-token"> tag on rendered pages and in the
// login form's hidden authenticity_token input; we try both.
func (client *Bl3Client) getCsrfToken(pageUrl string) (string, error) {
	res, err := client.Get(pageUrl)
	if err != nil {
		return "", err
	}
	doc, err := res.BodyAsHtmlDoc()
	if err != nil {
		return "", err
	}
	if token, ok := doc.Find(`meta[name="csrf-token"]`).Attr("content"); ok && token != "" {
		return token, nil
	}
	if token, ok := doc.Find(`input[name="authenticity_token"]`).Attr("value"); ok && token != "" {
		return token, nil
	}
	return "", errors.New("csrf token not found")
}

// redemptionToken returns a session CSRF token for redemption requests, fetching
// it once from the redemption page and caching it. The token is sent only as the
// X-CSRF-Token header on GET requests (which Rails does not CSRF-validate), so a
// single cached value is reused for the whole run; each redemption POST uses the
// per-form authenticity_token instead.
func (client *Bl3Client) redemptionToken() (string, error) {
	if client.csrfToken != "" {
		return client.csrfToken, nil
	}
	token, err := client.getCsrfToken(client.Config.BaseUrl + "/code_redemptions/new")
	if err != nil {
		return "", err
	}
	client.csrfToken = token
	return token, nil
}

// Login authenticates against the GearBox SHiFT website: fetch the login page
// for a CSRF token, then POST the credentials. On success the session cookie is
// stored in the client's cookie jar and used for subsequent requests.
func (client *Bl3Client) Login(username string, password string) error {
	token, err := client.getCsrfToken(client.Config.HomeUrl)
	if err != nil {
		return errors.New("failed to load login page: " + err.Error())
	}
	client.logf("got login csrf token (%d chars)", len(token))

	form := url.Values{}
	form.Set("utf8", "✓")
	form.Set("authenticity_token", token)
	form.Set("user[email]", username)
	form.Set("user[password]", password)
	form.Set("commit", "SIGN IN")

	res, err := client.PostForm(client.Config.LoginUrl, form, map[string]string{
		"Referer": client.Config.HomeUrl,
	})
	if err != nil {
		return errors.New("failed to submit login credentials: " + err.Error())
	}
	defer func() { _ = res.Body.Close() }()
	client.logf("POST %s -> %d", client.Config.LoginUrl, res.StatusCode)

	switch res.StatusCode {
	case http.StatusFound, http.StatusSeeOther, http.StatusMovedPermanently:
		location := res.Header.Get("Location")
		// A failed login bounces back to the home page with redirect_to=false.
		if strings.Contains(location, "redirect_to=false") {
			return errors.New("login failed - invalid email or password")
		}
		// Success: the cookie jar already captured the new _session_id.
		return nil
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w during login (status 429); please try again later", ErrRateLimited)
	case http.StatusServiceUnavailable:
		return errors.New("SHiFT login service is temporarily unavailable (503); please try again later")
	case http.StatusOK:
		// SHiFT re-renders the login form (200) instead of redirecting on failure.
		return errors.New("login failed - invalid email or password")
	default:
		return errors.New("unexpected login response status: " + strconv.Itoa(res.StatusCode))
	}
}
