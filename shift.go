package bl3auto

import (
	"errors"
)

type ShiftConfig struct {
	CodeListUrl   string `json:"codeListUrl"`
	CodeInfoUrl   string `json:"codeInfoUrl"`
	UserInfoUrl   string `json:"userInfoUrl"`
	GameCodename  string `json:"gameCodename"`
	AllowInactive bool
}

type ShiftCodeMap map[string][]string

func (codeMap ShiftCodeMap) Contains(code, platform string) bool {
	platforms, found := codeMap[code]
	if !found {
		return false
	}
	for _, p := range platforms {
		if p == platform {
			return true
		}
	}
	return false
}

type shiftCode struct {
	Game     string `json:"offer_title"`
	Platform string `json:"offer_service"`
	Active   bool   `json:"is_active"`
}

type shiftCodeFromList struct {
	Code     string `json:"code"`
	Platform string `json:"platform"`
}

func (client *Bl3Client) GetCodePlatforms(code string) ([]string, bool) {
	// For the SHiFT web interface, we'll assume codes work on common platforms
	// This is a simplified approach since we'd need to parse the rewards page HTML
	// to get the actual supported platforms for each code
	platforms := []string{"steam", "epic", "psn", "xbl"}
	return platforms, true
}

func (client *Bl3Client) RedeemShiftCode(code, platform string) error {
	// Note: The SHiFT web interface has changed significantly.
	// The old API-based redemption no longer works.
	// This function now returns an informative error message.
	
	return errors.New("SHiFT code redemption through the web API is no longer supported. " +
		"Please redeem codes manually at https://shift.gearboxsoftware.com/rewards. " +
		"The 2K Borderlands API has been discontinued.")
}

func (client *Bl3Client) GetShiftPlatforms() (StringSet, error) {
	platforms := StringSet{}

	response, err := client.Get(client.Config.Shift.UserInfoUrl)
	if err != nil {
		return platforms, errors.New("failed to get rewards page")
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		return platforms, errors.New("failed to access rewards page - not authenticated")
	}

	// For now, return common platforms - we'll need to parse the HTML to get actual platforms
	// This is a simplified approach since the SHiFT website structure may vary
	platforms.Add("steam")
	platforms.Add("epic")
	platforms.Add("psn")
	platforms.Add("xbl")
	
	return platforms, nil
}

func (client *Bl3Client) GetFullShiftCodeList() (ShiftCodeMap, error) {
	codeMap := ShiftCodeMap{}
	httpClient, err := NewHttpClient()
	if err != nil {
		return codeMap, err
	}

	res, err := httpClient.Get(client.Config.Shift.CodeListUrl)
	if err != nil {
		return codeMap, errors.New("failed to get SHiFT code list")
	}

	json, err := res.BodyAsJson()
	if err != nil {
		return codeMap, errors.New("failed to get SHiFT code list body as JSON")
	}

	codes := make([]shiftCodeFromList, 0)
	json.From("[0].codes").Select("code", "platform").Out(&codes)
	for _, code := range codes {
		platforms, valid := client.GetCodePlatforms(code.Code)
		if valid {
			codeMap[code.Code] = platforms
		}
	}

	return codeMap, nil
}
