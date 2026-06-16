// Command bl3auto automatically redeems Gearbox SHiFT codes for the Borderlands
// and Wonderlands games against your SHiFT account (https://shift.gearboxsoftware.com).
//
// Usage:
//
//	bl3auto [flags]
//
// Flags:
//
//	-e, --email <email>     SHiFT account email (prompted if omitted)
//	-p, --password <pw>     SHiFT account password (prompted if omitted)
//	    --shift-code <code> Redeem a single SHiFT code instead of the full list
//	    --allow-inactive    Attempt to redeem inactive SHiFT codes too
//	    --v1                Force the original orcicorn code source
//	    --v2                Force the newer ugoogalizer/mentalmars code source
//	    --platform <list>   Comma-separated services to redeem on
//	                        (steam,epic,psn,xboxlive,nintendo,stadia); default: all offered
//	    --config <path>     Use a local config.json instead of the published remote config
//	    --dryrun            Discover and match codes but do not redeem (no side effects)
//	-v, --verbose           Verbose step-level logging to stderr
//	-h, --help              Show this help
//
// By default the newer (v2) code source is used and the tool falls back to the
// original (v1) source only if v2 is unavailable. --v1/--v2 force a single source.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	bl3 "github.com/jauderho/bl3auto"
	"github.com/shibukawa/configdir"
)

// Request pacing and rate-limit backoff (sensible defaults; not user-tunable).
// Spacing requests and backing off on HTTP 429/503 lets us redeem in bulk
// without tripping SHiFT's rate limits or risking a ban.
var (
	throttleBase      = 400 * time.Millisecond // minimum spacing between SHiFT requests
	throttleJitter    = 400 * time.Millisecond // added random spacing (0..jitter)
	rateLimitBaseWait = 2 * time.Second        // first backoff on a 429/503
	rateLimitMaxWait  = 30 * time.Second       // backoff ceiling
	rateLimitRetries  = 5                      // give up (stop the run) after this many
)

// bulkThreshold is the candidate-code count at or above which a run is treated
// as "bulk": only then do we pace requests and back off on rate limits. A
// single --shift-code redemption stays fast and un-throttled.
const bulkThreshold = 5

// withBackoff runs op. When retry is true and op reports ErrRateLimited, it
// retries with exponential backoff and returns stop=true if the limit persists
// past rateLimitRetries (the caller should then halt the run). When retry is
// false (small/non-bulk runs) a rate-limit error is returned as-is.
func withBackoff(retry bool, op func() error) (err error, stop bool) {
	wait := rateLimitBaseWait
	for attempt := 1; ; attempt++ {
		err = op()
		if err == nil || !errors.Is(err, bl3.ErrRateLimited) {
			return err, false
		}
		if !retry {
			return err, false
		}
		if attempt > rateLimitRetries {
			return err, true
		}
		fmt.Printf("Rate limited by SHiFT; backing off %s (retry %d/%d) . . .\n", wait, attempt, rateLimitRetries)
		time.Sleep(wait)
		if wait *= 2; wait > rateLimitMaxWait {
			wait = rateLimitMaxWait
		}
	}
}

// gross but effective for now
const version = "2.3.0"

const SUCCESS = "success!"
const NOTFOUND = "not found."
const DOTDOTDOT = "' . . . . . "

var usernameHash string

// shiftOptions carries the resolved CLI options into the redemption routine.
type shiftOptions struct {
	source          string // "v1", "v2", or "" for default failover
	singleShiftCode string
	platformFilter  []string
	dryrun          bool
}

func usage() {
	out := flag.CommandLine.Output()
	_, _ = fmt.Fprintf(out, `bl3auto - automatically redeem Gearbox SHiFT codes

Usage:
  bl3auto [flags]

Flags:
  -e, --email <email>     SHiFT account email (prompted if omitted)
  -p, --password <pw>     SHiFT account password (prompted if omitted)
      --shift-code <code> Redeem a single SHiFT code instead of the full list
      --allow-inactive    Also attempt codes flagged as expired/inactive (v2 source)
      --v1                Force the original orcicorn code source
      --v2                Force the newer ugoogalizer/mentalmars code source
      --platform <list>   Comma-separated services to redeem on; default: all offered
                          (valid: steam, epic, psn, xboxlive, nintendo, stadia)
      --config <path>     Use a local config.json instead of the published remote config
      --dryrun            Discover and match codes but do not redeem (no side effects)
  -v, --verbose           Verbose step-level logging to stderr
  -h, --help              Show this help

Code source:
  By default the newer (v2) source is used, falling back to the original (v1)
  source only if v2 is unavailable. Use --v1 or --v2 to force a single source.

Examples:
  # Redeem all current codes for every linked platform
  bl3auto -e you@example.com -p 'secret'

  # See what would be redeemed without redeeming anything
  bl3auto -e you@example.com -p 'secret' --dryrun -v

  # Redeem a single code on Steam only
  bl3auto -e you@example.com -p 'secret' --shift-code ABCDE-... --platform steam

  # Force the original (v1) code source
  bl3auto -e you@example.com -p 'secret' --v1
`)
}

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

// summarize collapses whitespace and truncates a (possibly HTML-derived) status
// message to a single readable line.
func summarize(s string) string {
	joined := strings.Join(strings.Fields(s), " ")
	if joined == "" {
		return "not redeemable"
	}
	if len(joined) > 160 {
		return joined[:160] + "..."
	}
	return joined
}

// readRedeemedCodes parses the previously-redeemed-codes cache from a config
// folder — the directory the Docker `codes/` volume maps onto. A nil folder
// (no cache dir yet) or an unreadable/empty file yields an empty map.
func readRedeemedCodes(folder *configdir.Config, configFilename string) bl3.ShiftCodeMap {
	redeemedCodes := bl3.ShiftCodeMap{}
	if folder == nil {
		return redeemedCodes
	}
	data, err := folder.ReadFile(configFilename)
	if err != nil {
		return redeemedCodes
	}
	if j := bl3.JsonFromBytes(data); j != nil {
		j.Out(&redeemedCodes)
	}
	return redeemedCodes
}

// writeRedeemedCodes persists the redeemed-codes cache to a config folder.
func writeRedeemedCodes(folder *configdir.Config, configFilename string, redeemedCodes bl3.ShiftCodeMap) error {
	data, err := json.MarshalIndent(&redeemedCodes, "", "  ")
	if err != nil {
		return err
	}
	return folder.WriteFile(configFilename, data)
}

func loadRedeemedCodes(configDirs configdir.ConfigDir, configFilename string) bl3.ShiftCodeMap {
	fmt.Print("Getting previously redeemed SHIFT codes . . . . . ")
	folder := configDirs.QueryFolderContainsFile(configFilename)
	if folder == nil {
		fmt.Println(NOTFOUND)
		return bl3.ShiftCodeMap{}
	}
	fmt.Println(SUCCESS)
	return readRedeemedCodes(folder, configFilename)
}

func doShift(client *bl3.Bl3Client, opts shiftOptions) {
	configDirs := configdir.New("bl3auto", "bl3auto")
	configFilename := usernameHash + "-shift-codes.json"
	redeemedCodes := loadRedeemedCodes(configDirs, configFilename)

	// Gather the candidate codes: a single code, or the full list from the source.
	var codes []bl3.ShiftCode
	if opts.singleShiftCode != "" {
		code := strings.TrimSpace(strings.ToUpper(opts.singleShiftCode))
		codes = []bl3.ShiftCode{{Code: code}}
		fmt.Println("Checking single SHIFT code '" + code + "'")
	} else {
		label := "Getting new SHIFT codes"
		if opts.source != "" {
			label += " (" + opts.source + ")"
		}
		fmt.Print(label + " . . . . . ")
		list, err := client.GetShiftCodes(opts.source)
		if err != nil {
			printError(err)
			return
		}
		codes = list
		fmt.Println(SUCCESS)
	}

	// Only pace requests and back off on rate limits for bulk runs; a single
	// (or handful of) code(s) stays fast and un-throttled.
	bulk := len(codes) >= bulkThreshold
	if bulk {
		client.SetThrottle(throttleBase, throttleJitter)
	}

	redeemedAny := false
	cacheDirty := false
	rateLimited := false

codeLoop:
	for _, sc := range codes {
		code := sc.Code

		var forms []bl3.RedemptionForm
		var reason string
		err, stop := withBackoff(bulk, func() error {
			var e error
			forms, reason, e = client.GetCodeRedemptionForms(code)
			return e
		})
		if stop {
			rateLimited = true
			break codeLoop
		}
		if err != nil {
			fmt.Println("Skipping '" + code + "': " + err.Error())
			continue
		}
		if len(forms) == 0 {
			fmt.Println("'" + code + "': " + summarize(reason))
			continue
		}

		for _, form := range forms {
			if !bl3.ServiceMatches(opts.platformFilter, form.Service) {
				continue
			}
			if redeemedCodes.Contains(code, form.Service) {
				if opts.singleShiftCode != "" {
					fmt.Println("The single SHIFT code has already been redeemed on the '" + form.Service + "' platform")
					redeemedAny = true
				}
				continue
			}

			redeemedAny = true
			if opts.dryrun {
				fmt.Println("[dryrun] would redeem '" + form.Service + "' SHIFT code '" + code + "'")
				continue
			}

			fmt.Print("Trying '" + form.Service + "' SHIFT code '" + code + DOTDOTDOT)
			rerr, stop := withBackoff(bulk, func() error { return client.RedeemForm(form) })
			if stop {
				fmt.Println("rate limited.")
				rateLimited = true
				break codeLoop
			}
			if rerr != nil {
				fmt.Println(rerr)
				low := strings.ToLower(rerr.Error())
				if strings.Contains(low, "already") || strings.Contains(low, "expired") {
					redeemedCodes[code] = append(redeemedCodes[code], form.Service)
					cacheDirty = true
				}
			} else {
				redeemedCodes[code] = append(redeemedCodes[code], form.Service)
				cacheDirty = true
				fmt.Println(SUCCESS)
			}
		}
	}

	if rateLimited {
		fmt.Println("Stopped early due to repeated SHiFT rate limiting. Progress saved; re-run later to continue.")
	} else if !redeemedAny {
		if opts.singleShiftCode != "" {
			fmt.Println("The single SHIFT code could not be redeemed at this time. Try again later.")
		} else {
			fmt.Println("No new SHIFT codes at this time. Try again later.")
		}
		return
	}

	if cacheDirty && !opts.dryrun {
		folders := configDirs.QueryFolders(configdir.Global)
		if err := writeRedeemedCodes(folders[0], configFilename, redeemedCodes); err != nil {
			printError(err)
		}
	}
}

func main() {
	var (
		username        string
		password        string
		singleShiftCode string
		allowInactive   bool
		useV1           bool
		useV2           bool
		platform        string
		configPath      string
		dryrun          bool
		verbose         bool
	)

	flag.StringVar(&username, "e", "", "SHiFT account email")
	flag.StringVar(&username, "email", "", "SHiFT account email")
	flag.StringVar(&password, "p", "", "SHiFT account password")
	flag.StringVar(&password, "password", "", "SHiFT account password")
	flag.StringVar(&singleShiftCode, "shift-code", "", "Redeem a single SHiFT code instead of the full list")
	flag.BoolVar(&allowInactive, "allow-inactive", false, "Also attempt codes flagged as expired/inactive (v2 source)")
	flag.BoolVar(&useV1, "v1", false, "Force the original orcicorn code source")
	flag.BoolVar(&useV2, "v2", false, "Force the newer ugoogalizer/mentalmars code source")
	flag.StringVar(&platform, "platform", "", "Comma-separated services to redeem on (default: all offered)")
	flag.StringVar(&configPath, "config", "", "Use a local config.json instead of the published remote config")
	flag.BoolVar(&dryrun, "dryrun", false, "Discover and match codes but do not redeem")
	flag.BoolVar(&verbose, "verbose", false, "Verbose step-level logging to stderr")
	flag.BoolVar(&verbose, "v", false, "Verbose step-level logging to stderr")
	flag.Usage = usage
	flag.Parse()

	if useV1 && useV2 {
		fmt.Println("error: --v1 and --v2 are mutually exclusive")
		flag.Usage()
		os.Exit(2)
	}
	source := ""
	switch {
	case useV1:
		source = "v1"
	case useV2:
		source = "v2"
	}

	var platformFilter []string
	for p := range strings.SplitSeq(platform, ",") {
		if t := strings.TrimSpace(p); t != "" {
			platformFilter = append(platformFilter, t)
		}
	}

	if username == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter username (email): ")
		line, _, _ := reader.ReadLine()
		username = string(line)
	}
	if password == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter password        : ")
		line, _, _ := reader.ReadLine()
		password = string(line)
	}

	// SHA256 Hash
	hasher := sha256.New()
	hasher.Write([]byte(username))
	usernameHash = hex.EncodeToString(hasher.Sum(nil))

	fmt.Print("Setting up . . . . . ")
	client, err := bl3.NewBl3Client(configPath)
	if err != nil {
		printError(err)
		return
	}
	client.Verbose = verbose
	client.Config.Shift.AllowInactive = allowInactive
	fmt.Println(SUCCESS)

	if client.Config.Version != version {
		fmt.Println("Your version (" + version + ") is out of date. Please consider downloading the latest version (" + client.Config.Version + ") at https://github.com/jauderho/bl3auto/releases/latest")
	}

	fmt.Print("Logging in as '" + username + DOTDOTDOT)
	if err = client.Login(username, password); err != nil {
		printError(err)
		return
	}
	fmt.Println(SUCCESS)

	doShift(client, shiftOptions{
		source:          source,
		singleShiftCode: singleShiftCode,
		platformFilter:  platformFilter,
		dryrun:          dryrun,
	})

	exit()
}
