package bl3auto

import (
	"github.com/thedevsaddam/gojsonq/v2"
)

func JsonFromString(s string) *gojsonq.JSONQ {
	return gojsonq.New().JSONString(s)
}

func JsonFromBytes(bytes []byte) *gojsonq.JSONQ {
	return JsonFromString(string(bytes))
}

type Bl3Config struct {
	Version        string            `json:"version"`
	BaseUrl        string            `json:"baseUrl"`
	HomeUrl        string            `json:"homeUrl"`
	LoginUrl       string            `json:"loginUrl"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	Shift          ShiftConfig       `json:"shiftConfig"`
}
