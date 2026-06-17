package bl3auto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type ShiftConfig struct {
	CodeListUrlV1     string `json:"codeListUrlV1"`
	CodeListUrlV2     string `json:"codeListUrlV2"`
	RedemptionInfoUrl string `json:"redemptionInfoUrl"`
	RedemptionUrl     string `json:"redemptionUrl"`
	AllowInactive     bool
}

// ShiftCode is a single code parsed from either code-list format.
type ShiftCode struct {
	Code     string `json:"code"`
	Game     string `json:"game"`
	Platform string `json:"platform"`
	Reward   string `json:"reward"`
	Expired  bool   `json:"expired"`
}

// ShiftCodeMap tracks which services a code has been redeemed on (the on-disk cache).
type ShiftCodeMap map[string][]string

func (codeMap ShiftCodeMap) Contains(code, service string) bool {
	services, found := codeMap[code]
	if !found {
		return false
	}
	return slices.Contains(services, service)
}

// RedemptionForm is a single redeemable platform/service offered for a code,
// along with the hidden form fields required to submit the redemption.
type RedemptionForm struct {
	Service string
	Fields  map[string]string
}

// parseCodeList parses either SHiFT code-list format. Both are a top-level array
// whose first element holds a "codes" array. When dropExpired is set (the v2
// format, which carries an "expired" flag) expired entries are filtered out.
func parseCodeList(body []byte, dropExpired bool) []ShiftCode {
	parsed := make([]ShiftCode, 0)
	JsonFromBytes(body).From("[0].codes").
		Select("code", "game", "platform", "reward", "expired").Out(&parsed)

	codes := make([]ShiftCode, 0, len(parsed))
	for _, c := range parsed {
		if c.Code == "" || (dropExpired && c.Expired) {
			continue
		}
		codes = append(codes, c)
	}
	return codes
}

// GetShiftCodesV2 fetches and parses the newer ugoogalizer/mentalmars format.
// Codes flagged as expired are filtered out unless AllowInactive is set.
func (client *Bl3Client) GetShiftCodesV2() ([]ShiftCode, error) {
	body, err := fetchBytes(client.Config.Shift.CodeListUrlV2)
	if err != nil {
		return nil, errors.New("failed to get v2 SHiFT code list: " + err.Error())
	}
	return parseCodeList(body, !client.Config.Shift.AllowInactive), nil
}

// GetShiftCodesV1 fetches and parses the original orcicorn format. This format
// has no "expired" flag, so all codes are returned (the SHiFT site rejects
// stale ones at redemption time).
func (client *Bl3Client) GetShiftCodesV1() ([]ShiftCode, error) {
	body, err := fetchBytes(client.Config.Shift.CodeListUrlV1)
	if err != nil {
		return nil, errors.New("failed to get v1 SHiFT code list: " + err.Error())
	}
	return parseCodeList(body, false), nil
}

// GetShiftCodes returns codes for the requested source. "v1"/"v2" force a single
// source; any other value uses the default failover: try v2 first, fall back to
// v1 if v2 errors or yields no codes. Results are de-duplicated by code.
func (client *Bl3Client) GetShiftCodes(source string) ([]ShiftCode, error) {
	switch source {
	case "v1":
		codes, err := client.GetShiftCodesV1()
		return dedupeCodes(codes), err
	case "v2":
		codes, err := client.GetShiftCodesV2()
		return dedupeCodes(codes), err
	default:
		codes, err := client.GetShiftCodesV2()
		if err == nil && len(codes) > 0 {
			return dedupeCodes(codes), nil
		}
		if err != nil {
			client.logf("v2 code source failed (%v); falling back to v1", err)
		} else {
			client.logf("v2 code source returned no codes; falling back to v1")
		}
		v1, v1err := client.GetShiftCodesV1()
		return dedupeCodes(v1), v1err
	}
}

func dedupeCodes(codes []ShiftCode) []ShiftCode {
	seen := map[string]struct{}{}
	out := make([]ShiftCode, 0, len(codes))
	for _, c := range codes {
		key := strings.ToUpper(strings.TrimSpace(c.Code))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		c.Code = key
		out = append(out, c)
	}
	return out
}

// ServiceMatches reports whether a service value should be redeemed given the
// (possibly empty) platform filter. An empty filter matches everything; a
// non-empty filter matches when any token is a substring of the service value
// (e.g. filter "psn" matches service "psn" or "psn_..."), mirroring autoshift.
func ServiceMatches(filter []string, service string) bool {
	if len(filter) == 0 {
		return true
	}
	service = strings.ToLower(service)
	for _, f := range filter {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != "" && strings.Contains(service, f) {
			return true
		}
	}
	return false
}

// GetCodeRedemptionForms queries the SHiFT site for a code and returns one
// RedemptionForm per available service. When no redeemable form is present it
// returns an empty slice and a human-readable reason (already redeemed, expired,
// not available for this account, ...).
func (client *Bl3Client) GetCodeRedemptionForms(code string) ([]RedemptionForm, string, error) {
	token, err := client.redemptionToken()
	if err != nil {
		return nil, "", errors.New("failed to get redemption token (are you logged in?): " + err.Error())
	}

	res, err := client.GetWithHeaders(
		client.Config.Shift.RedemptionInfoUrl+"?code="+url.QueryEscape(code),
		map[string]string{
			"X-CSRF-Token":     token,
			"X-Requested-With": "XMLHttpRequest",
		},
	)
	if err != nil {
		return nil, "", errors.New("failed to query code: " + err.Error())
	}
	bodyBytes, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	client.logf("GET %s?code=%s -> %d", client.Config.Shift.RedemptionInfoUrl, code, res.StatusCode)

	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		return nil, "", &RateLimitError{Status: res.StatusCode, RetryAfter: parseRetryAfter(res.Header)}
	}
	if res.StatusCode != 200 {
		// A non-200 here is either an invalid code (e.g. 5xx) or, commonly, a 302
		// redirect when SHiFT is soft rate-limiting / shadowbanning us (or bouncing
		// us to sign in). The typed error carries the redirect target and any
		// Retry-After so the caller can classify it and back off (see --rampup).
		return nil, "", &CodeQueryStatusError{
			Status:     res.StatusCode,
			Location:   res.Header.Get("Location"),
			RetryAfter: parseRetryAfter(res.Header),
		}
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", errors.New("failed to parse redemption page")
	}

	if doc.Find("form.new_archway_code_redemption").Length() == 0 {
		// No form: the body text is the reason (an alert message).
		return nil, strings.TrimSpace(doc.Text()), nil
	}

	forms := make([]RedemptionForm, 0)
	doc.Find("form.new_archway_code_redemption").Each(func(_ int, form *goquery.Selection) {
		service, ok := form.Find("input#archway_code_redemption_service").Attr("value")
		if !ok || service == "" {
			return
		}
		fields := map[string]string{}
		form.Find("input").Each(func(_ int, inp *goquery.Selection) {
			name, hasName := inp.Attr("name")
			if !hasName || name == "" {
				return
			}
			value, _ := inp.Attr("value")
			fields[name] = value
		})
		forms = append(forms, RedemptionForm{Service: service, Fields: fields})
	})

	return forms, "", nil
}

// RedeemForm submits a single redemption form and resolves its outcome. It
// returns nil on success; otherwise an error whose message indicates the reason
// (contains "already" / "expired" / etc. so callers can classify it).
func (client *Bl3Client) RedeemForm(form RedemptionForm) error {
	data := url.Values{}
	for k, v := range form.Fields {
		data.Set(k, v)
	}

	res, err := client.PostForm(client.Config.Shift.RedemptionUrl, data, map[string]string{
		"Referer": client.Config.Shift.RedemptionUrl + "/new",
	})
	if err != nil {
		return errors.New("failed to submit redemption: " + err.Error())
	}
	return client.resolveRedemption(res)
}

// resolveRedemption follows the post-redemption redirect chain and reads the
// final status (either a polled JSON status or an inline alert).
func (client *Bl3Client) resolveRedemption(res *HttpResponse) error {
	visitedRedemption := false

	for range 10 {
		switch res.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			ra := parseRetryAfter(res.Header)
			_ = res.Body.Close()
			return &RateLimitError{Status: res.StatusCode, RetryAfter: ra}
		case 301, 302, 303:
			location := res.Header.Get("Location")
			_ = res.Body.Close()
			if location == "" {
				return errors.New("redemption redirected without a location")
			}
			if strings.Contains(location, "code_redemptions/") {
				visitedRedemption = true
			}
			next, err := client.Get(client.absoluteUrl(location))
			if err != nil {
				return errors.New("failed to follow redemption redirect: " + err.Error())
			}
			res = next
			continue
		default:
			bodyBytes, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
			if err != nil {
				return errors.New("failed to parse redemption response")
			}
			if div := doc.Find("#check_redemption_status"); div.Length() > 0 {
				if dataUrl, ok := div.Attr("data-url"); ok && dataUrl != "" {
					return client.pollRedemptionStatus(dataUrl)
				}
			}
			return statusFromText(doc.Find(".alert").Text(), visitedRedemption)
		}
	}
	return errors.New("too many redemption redirects")
}

// pollRedemptionStatus polls the async status endpoint until it reports a result
// (or times out). The endpoint returns JSON with a "text" field once resolved.
func (client *Bl3Client) pollRedemptionStatus(dataUrl string) error {
	token, _ := client.redemptionToken()
	full := client.absoluteUrl(dataUrl)

	for range 6 {
		res, err := client.GetWithHeaders(full, map[string]string{
			"X-CSRF-Token":     token,
			"X-Requested-With": "XMLHttpRequest",
			"Accept":           "application/json, text/javascript, */*; q=0.01",
		})
		if err != nil {
			return errors.New("failed to check redemption status: " + err.Error())
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()

		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Text != "" {
			return statusFromText(payload.Text, true)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("redemption still pending - launch a SHiFT-enabled title and try again later")
}

// statusFromText maps a SHiFT status/alert message to a result. nil means the
// code was (or already was) redeemed successfully.
func statusFromText(text string, visitedRedemption bool) error {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch {
	case lower == "":
		if visitedRedemption {
			return nil
		}
		return errors.New("redemption returned no status; try again later")
	case strings.Contains(lower, "success"):
		return nil
	case strings.Contains(lower, "already") && strings.Contains(lower, "redeem"):
		return errors.New("this SHiFT code has already been redeemed")
	case strings.Contains(lower, "expired"):
		return errors.New("this SHiFT code has expired")
	case strings.Contains(lower, "not available"):
		return errors.New("this SHiFT code is not available for your account")
	case strings.Contains(lower, "launch a shift"):
		return errors.New("launch a SHiFT-enabled title first, then try again later")
	default:
		return errors.New(strings.TrimSpace(text))
	}
}

func (client *Bl3Client) absoluteUrl(ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return client.Config.BaseUrl + "/" + strings.TrimPrefix(ref, "/")
}
