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
//	    --rampup            Cautious mode for a first run / long gap: pace requests,
//	                        back off after 5 consecutive non-200s, stop after 20
//	    --migrate           Upgrade the redeemed-codes cache file in place and exit
//	                        (no login); -e selects the per-account cache
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

// --rampup tuning. Rampup is the cautious mode for a first run or after a long gap,
// where SHiFT readily soft-rate-limits us with 302s on the code-query. It paces
// requests much more slowly and reacts to *consecutive* non-200 responses: back off
// after rampupBackoffAfter in a row, and stop the run after rampupStopAfter (a likely
// shadowban). All are vars so tests can shrink them.
var (
	rampupThrottleBase   = 1500 * time.Millisecond // minimum spacing between requests in rampup
	rampupThrottleJitter = 1500 * time.Millisecond // added random spacing (0..jitter)
	rampupBackoffBase    = 5 * time.Second         // first backoff after rampupBackoffAfter
	rampupBackoffMax     = 60 * time.Second        // backoff ceiling
	rampupBackoffAfter   = 5                       // consecutive non-200 → start backing off
	rampupStopAfter      = 20                      // consecutive non-200 → stop the run
)

// bulkThreshold is the candidate-code count at or above which a run is treated
// as "bulk": only then do we pace requests and back off on rate limits. A
// single --shift-code redemption stays fast and un-throttled.
const bulkThreshold = 5

// resolveCacheFolder returns the folder that stores the redeemed-codes cache —
// the directory the Docker `codes/` volume maps onto. Overridable in tests.
var resolveCacheFolder = func() *configdir.Config {
	return configdir.New("bl3auto", "bl3auto").QueryFolders(configdir.Global)[0]
}

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
	rampup          bool
}

// cacheVersion is the current on-disk schema version of the redeemed-codes cache.
// Bump it when the layout changes so future migrations can branch on it.
const cacheVersion = 2

// redeemedCache is the versioned on-disk format of the redeemed-codes cache. Older
// files are a bare ShiftCodeMap (no wrapper); readRedeemedCache reads both.
type redeemedCache struct {
	Version int              `json:"version"`
	LastRun time.Time        `json:"lastRun"`
	Codes   bl3.ShiftCodeMap `json:"codes"`
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
      --rampup            Cautious mode for a first run or after a long gap: paces
                          requests, backs off after 5 consecutive non-200 responses,
                          and stops cleanly after 20 (likely rate-limit/shadowban)
      --migrate           Upgrade the redeemed-codes cache file in place to the
                          current version and exit (no login; -e selects the cache)
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

  # First run, or first in a long while: redeem cautiously
  bl3auto -e you@example.com -p 'secret' --rampup

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

// readRedeemedCache parses the previously-redeemed-codes cache from a config folder
// — the directory the Docker `codes/` volume maps onto. It reads both the current
// versioned format ({version, lastRun, codes}) and the older bare-map format,
// returning the codes, the last-run time (zero if unknown), and whether a cache file
// existed at all. A nil folder or unreadable/empty file yields (empty, zero, false).
func readRedeemedCache(folder *configdir.Config, configFilename string) (bl3.ShiftCodeMap, time.Time, bool) {
	if folder == nil {
		return bl3.ShiftCodeMap{}, time.Time{}, false
	}
	data, err := folder.ReadFile(configFilename)
	if err != nil {
		return bl3.ShiftCodeMap{}, time.Time{}, false
	}

	// Current format: a wrapper with a "codes" key. The old bare map has no such
	// key (SHiFT codes never look like "codes"), so Codes stays nil and we fall
	// through to the back-compat path.
	var cache redeemedCache
	if json.Unmarshal(data, &cache) == nil && cache.Codes != nil {
		return cache.Codes, cache.LastRun, true
	}

	// Back-compat: an old bare ShiftCodeMap. Recency is unknown (zero time).
	bare := bl3.ShiftCodeMap{}
	_ = json.Unmarshal(data, &bare)
	return bare, time.Time{}, true
}

// writeRedeemedCache persists the redeemed-codes cache in the current versioned
// format, stamping it with the given last-run time.
func writeRedeemedCache(folder *configdir.Config, configFilename string, codes bl3.ShiftCodeMap, lastRun time.Time) error {
	if folder == nil {
		return nil
	}
	cache := redeemedCache{Version: cacheVersion, LastRun: lastRun, Codes: codes}
	data, err := json.MarshalIndent(&cache, "", "  ")
	if err != nil {
		return err
	}
	return folder.WriteFile(configFilename, data)
}

func loadRedeemedCodes(folder *configdir.Config, configFilename string) (bl3.ShiftCodeMap, time.Time, bool) {
	fmt.Print("Getting previously redeemed SHIFT codes . . . . . ")
	codes, lastRun, existed := readRedeemedCache(folder, configFilename)
	if len(codes) == 0 {
		fmt.Println(NOTFOUND)
	} else {
		fmt.Println(SUCCESS)
	}
	return codes, lastRun, existed
}

// rampupAdvised nudges toward --rampup: a likely first run (no cache), an old-format
// cache with unknown recency, or a last run more than ~6 months ago.
func rampupAdvised(existed bool, lastRun, now time.Time) bool {
	if !existed || lastRun.IsZero() {
		return true
	}
	return lastRun.Before(now.AddDate(0, -6, 0))
}

// runMigrate upgrades the redeemed-codes cache file in place to the current versioned
// format. It is a standalone, login-free maintenance op (--migrate): it reads the
// existing file (including the old bare-map format), preserves its codes and last-run
// time, and rewrites it as {version, lastRun, codes}. lastRun is intentionally left
// untouched (zero for an old file) so the stale-run warning still fires for long-absent
// users. Already-current files are left as-is.
func runMigrate() {
	folder := resolveCacheFolder()
	configFilename := usernameHash + "-shift-codes.json"
	if folder == nil {
		fmt.Println("No cache folder available; nothing to migrate.")
		return
	}

	data, err := folder.ReadFile(configFilename)
	if err != nil {
		fmt.Println("No redeemed-codes cache to migrate.")
		return
	}
	var existing redeemedCache
	if json.Unmarshal(data, &existing) == nil && existing.Codes != nil && existing.Version == cacheVersion {
		fmt.Printf("Cache is already version %d (nothing to do).\n", cacheVersion)
		return
	}

	codes, lastRun, _ := readRedeemedCache(folder, configFilename)
	if err := writeRedeemedCache(folder, configFilename, codes, lastRun); err != nil {
		printError(err)
		return
	}
	fmt.Printf("Migrated %d codes to cache version %d (in place).\n", len(codes), cacheVersion)
}

func doShift(client *bl3.Bl3Client, opts shiftOptions) {
	cacheFolder := resolveCacheFolder()
	configFilename := usernameHash + "-shift-codes.json"
	redeemedCodes, lastRun, existed := loadRedeemedCodes(cacheFolder, configFilename)

	// Gather the candidate codes: a single code, or the full list from the source.
	var codes []bl3.ShiftCode
	if opts.singleShiftCode != "" {
		code := strings.TrimSpace(strings.ToUpper(opts.singleShiftCode))
		codes = []bl3.ShiftCode{{Code: code}}
		fmt.Println("Checking single SHIFT code '" + code + "'")
	} else {
		// Nudge first-time / long-absent users toward --rampup before a bulk run,
		// where SHiFT is quick to soft-rate-limit us with 302s.
		if !opts.rampup && rampupAdvised(existed, lastRun, time.Now()) {
			fmt.Println("WARNING: this looks like a first run or your first in a while.")
			fmt.Println("         SHiFT may rate-limit a large redemption. Consider re-running")
			fmt.Println("         with --rampup to pace requests and stop cleanly if throttled.")
		}

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

	// Pace requests and back off on rate limits for bulk runs; --rampup forces this
	// on (with much more conservative spacing) regardless of how many codes there
	// are. A single (or handful of) code(s) otherwise stays fast and un-throttled.
	bulk := opts.rampup || len(codes) >= bulkThreshold
	switch {
	case opts.rampup:
		client.SetThrottle(rampupThrottleBase, rampupThrottleJitter)
	case bulk:
		client.SetThrottle(throttleBase, throttleJitter)
	}

	redeemedAny := false
	rateLimited := false
	stoppedShadowban := false
	consecutive := 0 // consecutive non-200 code-query responses (see --rampup)

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
		// A non-200 code-query (commonly a 302) is SHiFT throttling us. In rampup we
		// count these: back off after a few in a row, and stop cleanly once it's
		// clearly a shadowban. A clean (200) response resets the counter.
		var statusErr *bl3.CodeQueryStatusError
		if errors.As(err, &statusErr) {
			consecutive++
			fmt.Println("Skipping '" + code + "': " + err.Error())
			if opts.rampup && consecutive >= rampupStopAfter {
				stoppedShadowban = true
				break codeLoop
			}
			if opts.rampup && consecutive >= rampupBackoffAfter {
				wait := rampupBackoff(consecutive)
				fmt.Printf("         %d non-200 responses in a row; backing off %s . . .\n", consecutive, wait)
				time.Sleep(wait)
			}
			continue
		}
		if err != nil {
			fmt.Println("Skipping '" + code + "': " + err.Error())
			continue
		}
		consecutive = 0
		if len(forms) == 0 {
			fmt.Println("'" + code + "': " + summarize(reason))
			continue
		}

		// One redemption per form: the SHiFT site returns a form for each platform
		// linked to the account, so a code is redeemed once per linked platform
		// (--platform narrows this set).
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
				}
			} else {
				redeemedCodes[code] = append(redeemedCodes[code], form.Service)
				fmt.Println(SUCCESS)
			}
		}
	}

	switch {
	case stoppedShadowban:
		fmt.Printf("Stopped after %d consecutive non-200 responses (likely rate-limited/shadowbanned by SHiFT).\n", consecutive)
		fmt.Println("Progress saved; wait a while and re-run with --rampup to continue.")
	case rateLimited:
		fmt.Println("Stopped early due to repeated SHiFT rate limiting. Progress saved; re-run later to continue.")
	case !redeemedAny:
		if opts.singleShiftCode != "" {
			fmt.Println("The single SHIFT code could not be redeemed at this time. Try again later.")
		} else {
			fmt.Println("No new SHIFT codes at this time. Try again later.")
		}
		// Still bump lastRun below so the stale-run warning tracks this attempt.
	}

	// On any non-dryrun run, rewrite the cache: this persists newly-redeemed codes,
	// bumps lastRun (powering the stale-run warning), and upgrades an old bare-map
	// file to the current versioned format.
	if !opts.dryrun {
		if err := writeRedeemedCache(cacheFolder, configFilename, redeemedCodes, time.Now()); err != nil {
			printError(err)
		}
	}
}

// rampupBackoff returns the escalating sleep for the Nth consecutive non-200 code
// query in rampup mode, capped at rampupBackoffMax.
func rampupBackoff(consecutive int) time.Duration {
	shift := consecutive - rampupBackoffAfter
	if shift < 0 {
		shift = 0
	}
	wait := rampupBackoffBase << uint(shift)
	if wait > rampupBackoffMax || wait <= 0 {
		wait = rampupBackoffMax
	}
	return wait
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
		rampup          bool
		migrate         bool
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
	flag.BoolVar(&rampup, "rampup", false, "Cautious mode for a first run / long gap: pace requests and stop cleanly if throttled")
	flag.BoolVar(&migrate, "migrate", false, "Upgrade the redeemed-codes cache file in place to the current version and exit (no login)")
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

	// SHA256 Hash (selects the per-account redeemed-codes cache file).
	hasher := sha256.New()
	hasher.Write([]byte(username))
	usernameHash = hex.EncodeToString(hasher.Sum(nil))

	// --migrate is a standalone, login-free maintenance op: just upgrade the cache.
	if migrate {
		runMigrate()
		return
	}

	if password == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter password        : ")
		line, _, _ := reader.ReadLine()
		password = string(line)
	}

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
		rampup:          rampup,
	})

	exit()
}
