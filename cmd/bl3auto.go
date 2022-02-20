package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	bl3 "github.com/jauderho/bl3auto"
	"github.com/shibukawa/configdir"
)

// gross but effective for now
const version = "2.2.14"

const SUCCESS = "success!"
const NOTFOUND = "not found."
const DOTDOTDOT = "' . . . . . "

var usernameHash string

func printError(err error) {
	fmt.Println("failed!")
	fmt.Print("Had error: ")
	fmt.Println(err)
}

func exit() {
	fmt.Print("Exiting in ")
	for i := 5; i > 0; i-- {
		fmt.Print(strconv.Itoa(i) + " ")
		time.Sleep(time.Second)
	}
	fmt.Println("")
}

func doShift(client *bl3.Bl3Client, singleShiftCode string) {
	fmt.Print("Getting SHIFT platforms . . . . . ")
	platforms, err := client.GetShiftPlatforms()
	if err != nil {
		printError(err)
		return
	}
	fmt.Println(SUCCESS)

	configDirs := configdir.New("bl3auto", "bl3auto")
	configFilename := usernameHash + "-shift-codes.json"
	redeemedCodes := bl3.ShiftCodeMap{}

	fmt.Print("Getting previously redeemed SHIFT codes . . . . . ")
	folder := configDirs.QueryFolderContainsFile(configFilename)
	if folder != nil {
		data, err := folder.ReadFile(configFilename)
		if err == nil {
			json := bl3.JsonFromBytes(data)
			if json != nil {
				json.Out(&redeemedCodes)
				fmt.Println(SUCCESS)
			} else {
				fmt.Println(NOTFOUND)
			}
		} else {
			fmt.Println(NOTFOUND)
		}
	} else {
		fmt.Println(NOTFOUND)
	}

	shiftCodes := bl3.ShiftCodeMap{}

	if singleShiftCode != "" {
		singleShiftCode = strings.TrimSpace(strings.ToUpper(singleShiftCode))
		fmt.Print("Checking single SHIFT code '" + singleShiftCode + DOTDOTDOT)
		platforms, valid := client.GetCodePlatforms(singleShiftCode)
		if valid {
			shiftCodes[singleShiftCode] = platforms
			fmt.Println(SUCCESS)
		} else {
			fmt.Println("no available redemption platforms found!")
		}
	} else {
		fmt.Print("Getting new SHIFT codes . . . . . ")
		allShiftCodes, err := client.GetFullShiftCodeList()
		if err != nil {
			printError(err)
			return
		}
		shiftCodes = allShiftCodes
		fmt.Println(SUCCESS)
	}

	foundCodes := false
	for code, codePlatforms := range shiftCodes {
		for _, platform := range codePlatforms {
			if _, found := platforms[platform]; found {
				if !redeemedCodes.Contains(code, platform) {
					foundCodes = true
					fmt.Print("Trying '" + platform + "' SHIFT code '" + code + DOTDOTDOT)
					err := client.RedeemShiftCode(code, platform)
					if err != nil {
						fmt.Println(err)
						if strings.Contains(strings.ToLower(err.Error()), "already") {
							redeemedCodes[code] = append(redeemedCodes[code], platform)
						} else if strings.Contains(strings.ToLower(err.Error()), "has expired") {
							redeemedCodes[code] = append(redeemedCodes[code], platform)
						}
					} else {
						redeemedCodes[code] = append(redeemedCodes[code], platform)
						fmt.Println(SUCCESS)
					}
				} else if singleShiftCode != "" {
					fmt.Println("The single SHIFT code has already been redeemed on the '" + platform + "' platform")
					foundCodes = true
				}
			}
		}
	}

	if !foundCodes && singleShiftCode != "" {
		fmt.Println("The single SHIFT code could not be redeemed at this time. Try again later.")
	} else if !foundCodes {
		fmt.Println("No new SHIFT codes at this time. Try again later.")
	} else {
		folders := configDirs.QueryFolders(configdir.Global)
		data, err := json.MarshalIndent(&redeemedCodes, "", "  ")
		if err == nil {
			err := folders[0].WriteFile(configFilename, data)
			printError(err)
		}
	}

}

func main() {
	username := ""
	password := ""
	singleShiftCode := ""
	allowInactive := false
	flag.StringVar(&username, "e", "", "Email")
	flag.StringVar(&username, "email", "", "Email")
	flag.StringVar(&password, "p", "", "Password")
	flag.StringVar(&password, "password", "", "Password")
	flag.StringVar(&singleShiftCode, "shift-code", "", "Single SHIFT code to redeem")
	flag.BoolVar(&allowInactive, "allow-inactive", false, "Attempt to redeem SHIFT codes even if they are inactive?")
	flag.Parse()

	if username == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter username (email): ")
		bytes, _, _ := reader.ReadLine()
		username = string(bytes)
	}
	if password == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter password        : ")
		bytes, _, _ := reader.ReadLine()
		password = string(bytes)
	}

	// SHA256 Hash
	hasher := sha256.New()
	hasher.Write([]byte(username))
	usernameHash = hex.EncodeToString(hasher.Sum(nil))

	fmt.Print("Setting up . . . . . ")
	client, err := bl3.NewBl3Client()
	if err != nil {
		printError(err)
		return
	}

	client.Config.Shift.AllowInactive = allowInactive

	fmt.Println(SUCCESS)

	if client.Config.Version != version {
		fmt.Println("Your version (" + version + ") is out of date. Please consider downloading the latest version (" + client.Config.Version + ") at https://github.com/jauderho/bl3auto/releases/latest")
	}

	fmt.Print("Logging in as '" + username + DOTDOTDOT)
	err = client.Login(username, password)
	if err != nil {
		printError(err)
		return
	}
	fmt.Println(SUCCESS)

	doShift(client, singleShiftCode)

	exit()
}
