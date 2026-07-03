package bl3auto

type Bl3Config struct {
	Version        string            `json:"version"`
	BaseUrl        string            `json:"baseUrl"`
	HomeUrl        string            `json:"homeUrl"`
	LoginUrl       string            `json:"loginUrl"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	Shift          ShiftConfig       `json:"shiftConfig"`
}
