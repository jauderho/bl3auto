package bl3auto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"

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
		},
		http.Header{
			"User-Agent": []string{"BL3 Auto SHiFT"},
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
	return &HttpResponse{
		*res,
	}, err
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

	res, err := client.Get("https://raw.githubusercontent.com/jauderho/bl3auto/main/config.json")
	if err != nil {
		return nil, errors.New("failed to get config")
	}

	configJson, err := res.BodyAsJson()
	if err != nil {
		return nil, errors.New("failed to get config")
	}
	config := Bl3Config{}
	configJson.Out(&config)

	for header, value := range config.RequestHeaders {
		client.SetDefaultHeader(header, value)
	}

	return &Bl3Client{
		HttpClient: *client,
		Config:     config,
	}, nil
}

func (client *Bl3Client) Login(username string, password string) error {
	data := map[string]string{
		"username": username,
		"password": password,
	}

	loginRes, err := client.PostJson(client.Config.LoginUrl, data)
	if err != nil {
		return errors.New("failed to submit login credentials")
	}
	defer loginRes.Body.Close()

	if loginRes.StatusCode != 200 {
		return errors.New("failed to login")
	}

	/* if loginRes.Header.Get(client.Config.LoginRedirectHeader) == "" {
		return errors.New("Failed to start session")
	}

	sessionRes, err := client.Get(loginRes.Header.Get(client.Config.LoginRedirectHeader))
	if err != nil {
		return errors.New("Failed to get session")
	}
	defer sessionRes.Body.Close()*/

	client.SetDefaultHeader(client.Config.SessionHeader, loginRes.Header.Get(client.Config.SessionIdHeader))
	return nil
}
