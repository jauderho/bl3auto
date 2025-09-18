package bl3auto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/thedevsaddam/gojsonq/v2"
)

type HttpClient struct {
	http.Client
	headers http.Header
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
		http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Don't follow redirects automatically - we want to handle them manually
				return http.ErrUseLastResponse
			},
		},
		http.Header{
			"User-Agent":      []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:109.0) Gecko/20100101 Firefox/119.0"},
			"Accept":          []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
			"Accept-Language": []string{"en-US,en;q=0.5"},
			"Accept-Encoding": []string{"gzip, deflate, br"},
			"DNT":             []string{"1"},
			"Connection":      []string{"keep-alive"},
			"Upgrade-Insecure-Requests": []string{"1"},
		},
	}, nil
}

func (response *HttpResponse) BodyAsHtmlDoc() (*goquery.Document, error) {
	defer response.Body.Close()

	if response.StatusCode != 200 {
		return nil, errors.New("invalid response code")
	}

	doc, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, errors.New("invalid html")
	}

	return doc, nil
}

func (response *HttpResponse) BodyAsJson() (*gojsonq.JSONQ, error) {
	defer response.Body.Close()

	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, errors.New("invalid response json")
	}

	return JsonFromBytes(bodyBytes), nil
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

func (client *HttpClient) Do(req *http.Request) (*HttpResponse, error) {
	for k, v := range client.headers {
		for _, x := range v {
			req.Header.Set(k, x)
		}
	}
	return getResponse(client.Client.Do(req))
}

func (client *HttpClient) Get(url string) (*HttpResponse, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (client *HttpClient) Head(url string) (*HttpResponse, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (client *HttpClient) Post(url, contentType string, body io.Reader) (*HttpResponse, error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return client.Do(req)
}

func (client *HttpClient) PostJson(url string, data interface{}) (*HttpResponse, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return client.Post(url, "application/json", bytes.NewBuffer(jsonData))
}

type Bl3Client struct {
	HttpClient
	Config Bl3Config
}

func NewBl3Client() (*Bl3Client, error) {
	client, err := NewHttpClient()
	if err != nil {
		return nil, errors.New("failed to start client")
	}

	// Load config from local file
	configBytes, err := os.ReadFile("config.json")
	if err != nil {
		return nil, errors.New("failed to read local config.json: " + err.Error())
	}

	config := Bl3Config{}
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		return nil, errors.New("failed to parse config.json: " + err.Error())
	}

	for header, value := range config.RequestHeaders {
		client.SetDefaultHeader(header, value)
	}

	return &Bl3Client{
		HttpClient: *client,
		Config:     config,
	}, nil
}

func (client *Bl3Client) Login(username string, password string) error {
	// First, get the login page to extract CSRF token
	homeRes, err := client.Get("https://shift.gearboxsoftware.com/home")
	if err != nil {
		return errors.New("failed to get login page")
	}
	defer homeRes.Body.Close()

	// Parse the HTML to extract CSRF token from the hidden form field
	doc, err := homeRes.BodyAsHtmlDoc()
	if err != nil {
		return errors.New("failed to parse login page")
	}

	// Get the authenticity token from the hidden form input (not the meta tag)
	csrfToken, exists := doc.Find("input[name='authenticity_token']").Attr("value")
	if !exists {
		return errors.New("failed to find authenticity token in form")
	}

	// Add a small delay to mimic human behavior
	time.Sleep(1 * time.Second)

	// Prepare form data using proper URL encoding
	formValues := url.Values{}
	formValues.Set("utf8", "✓")
	formValues.Set("authenticity_token", csrfToken)
	formValues.Set("user[email]", username)
	formValues.Set("user[password]", password)
	formValues.Set("commit", "SIGN IN")
	
	formData := formValues.Encode()

	// Create the POST request with proper headers
	req, err := http.NewRequest("POST", client.Config.LoginUrl, bytes.NewBufferString(formData))
	if err != nil {
		return errors.New("failed to create login request: " + err.Error())
	}
	
	// Set required headers for form submission
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://shift.gearboxsoftware.com/home")
	
	loginRes, err := client.Do(req)
	if err != nil {
		return errors.New("failed to submit login credentials: " + err.Error())
	}
	defer loginRes.Body.Close()

	// Check for successful login (should be a redirect)
	if loginRes.StatusCode == 503 {
		return errors.New("SHiFT login service is temporarily unavailable (503). This may be due to rate limiting, maintenance, or the service being overloaded. Please try again later.")
	}
	
	// Check for successful login - should be 302 redirect
	if loginRes.StatusCode == 302 {
		location := loginRes.Header.Get("Location")
		
		// Check if this is a failed login redirect (back to home with redirect_to=false)
		if bytes.Contains([]byte(location), []byte("home?redirect_to=false")) {
			return errors.New("login failed - invalid credentials (redirected back to login page)")
		}
		
		// If it's a redirect to somewhere else, it's likely successful
		if location != "" {
			// Extract session cookie from response
			cookies := loginRes.Header.Values("Set-Cookie")
			for _, cookie := range cookies {
				if len(cookie) >= 12 && cookie[:12] == "_session_id=" {
					// Extract just the session cookie part
					sessionCookie := cookie
					if idx := bytes.IndexByte([]byte(cookie), ';'); idx != -1 {
						sessionCookie = cookie[:idx]
					}
					client.SetDefaultHeader("Cookie", sessionCookie)
					return nil
				}
			}
			// Even if no session cookie found, the redirect might indicate success
			return nil
		}
	}
	
	if loginRes.StatusCode != 302 && loginRes.StatusCode != 200 {
		return errors.New("unexpected login response status: " + fmt.Sprintf("%d", loginRes.StatusCode))
	}
	
	if loginRes.StatusCode != 302 {
		// Read the response body to get more details about the error
		bodyBytes, _ := io.ReadAll(loginRes.Body)
		bodyStr := string(bodyBytes)
		
		// Look for specific error messages in the response
		if bytes.Contains(bodyBytes, []byte("Invalid email or password")) || 
		   bytes.Contains(bodyBytes, []byte("invalid email or password")) ||
		   bytes.Contains(bodyBytes, []byte("Invalid credentials")) {
			return errors.New("invalid email or password - please check your credentials")
		}
		
		// Look for other common error patterns
		if bytes.Contains(bodyBytes, []byte("alert-danger")) ||
		   bytes.Contains(bodyBytes, []byte("error-message")) ||
		   bytes.Contains(bodyBytes, []byte("field_with_errors")) {
			return errors.New("login failed - form validation error or invalid credentials")
		}
		
		// Check if we're still on the login page (sign in form present)
		if bytes.Contains(bodyBytes, []byte("Sign in")) && bytes.Contains(bodyBytes, []byte("user[email]")) {
			return errors.New("login failed - still on login page, likely invalid credentials")
		}
		
		// If it's a 200 but not a redirect, it might be the login page with errors
		if loginRes.StatusCode == 200 {
			return errors.New("login failed - credentials may be invalid or additional verification required (status: 200, expected 302 redirect)")
		}
		
		maxLen := 200
		if len(bodyStr) < maxLen {
			maxLen = len(bodyStr)
		}
		return errors.New("failed to login - server error (status: " + fmt.Sprintf("%d", loginRes.StatusCode) + "). Expected 302 redirect for successful login. Response: " + bodyStr[:maxLen])
	}

	// Extract session cookie from response
	cookies := loginRes.Header.Values("Set-Cookie")
	for _, cookie := range cookies {
		if len(cookie) >= 12 && cookie[:12] == "_session_id=" {
			// Extract just the session cookie part
			sessionCookie := cookie
			if idx := bytes.IndexByte([]byte(cookie), ';'); idx != -1 {
				sessionCookie = cookie[:idx]
			}
			client.SetDefaultHeader("Cookie", sessionCookie)
			return nil
		}
	}

	return errors.New("failed to extract session cookie")
}
