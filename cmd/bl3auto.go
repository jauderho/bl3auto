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
//	    --game <list>       Comma-separated games to redeem (substring match; default: all)
//	    --skip-game <list>  Comma-separated games to skip (substring match)
//	    --config <path>     Use a local config.json instead of the published remote config
//	    --dryrun            Discover and match codes but do not redeem (no side effects)
//	    --rampup            Cautious mode for a first run / long gap: pace requests,
//	                        back off after 5 consecutive non-200s, stop after 20
//	    --count <n>         Stop and save after n successful redemptions (0 = no limit)
//	    --refresh           Re-query codes already redeemed on every linked platform
//	                        (to pick up platforms linked since)
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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	bl3 "github.com/jauderho/bl3auto"
	"golang.org/x/term"
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

// Consecutive non-200 (commonly 302) handling for bulk runs. SHiFT soft-rate-limits
// by redirecting the code-query, so on every bulk run we back off after backoffAfter
// such responses in a row and stop the run after stopAfter (a likely shadowban),
// saving progress. --rampup adds slower request pacing on top. All vars so tests can
// shrink them.
var (
	backoffAfter = 5               // consecutive non-200 → start backing off
	stopAfter    = 20              // consecutive non-200 → stop the run
	backoffBase  = 5 * time.Second // first backoff after backoffAfter
	backoffMax   = 60 * time.Second
)

// maxRetryAttempts caps how many times a 302'd (throttled) code is re-queued to be
// retried at the end of a bulk run before it is given up. Retries flow through the main
// loop and remain subject to its backoff/stop logic. A var so tests can change it.
var maxRetryAttempts = 5

// --rampup request pacing: much slower spacing than the normal bulk throttle, for a
// first run or after a long gap when SHiFT throttles aggressively.
var (
	rampupThrottleBase   = 1500 * time.Millisecond
	rampupThrottleJitter = 1500 * time.Millisecond
)

// Adaptive (AIMD) throttle. On a bulk run the request spacing self-tunes to the
// rate SHiFT tolerates: it widens multiplicatively on each non-200 code query (a
// throttle signal) up to throttleCeil, and narrows by throttleSpeedup after every
// throttleSpeedupAfter clean queries, never below the configured base. All vars so
// tests can shrink them.
var (
	throttleCeil         = 8 * time.Second
	throttleSlowFactor   = 1.5
	throttleSpeedup      = 200 * time.Millisecond
	throttleSpeedupAfter = 8
)

// bulkThreshold is the candidate-code count at or above which a run is treated
// as "bulk": only then do we pace requests and back off on rate limits. A
// single --shift-code redemption stays fast and un-throttled.
const bulkThreshold = 5

// checkpointEvery is how often (in new successful redemptions) the cache is
// flushed mid-run, bounding progress lost to a hard crash (e.g. kill -9) where
// the interrupt handler never runs. A var so tests can shrink it.
var checkpointEvery = 10

// sleepCtx sleeps for d, returning false if the context is cancelled first
// (e.g. the user pressed Ctrl-C). Used for the long loop-level backoff sleeps so
// an interrupt aborts promptly instead of waiting out the backoff.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// cacheFolder is a directory that stores the redeemed-codes cache, replacing the
// vendored configdir.Config with just what readRedeemedCache/writeRedeemedCache
// need: reading a file inside it and ensuring it exists on write.
type cacheFolder struct {
	path string
}

// ReadFile reads fileName from inside the folder.
func (f *cacheFolder) ReadFile(fileName string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.path, fileName))
}

// CreateParentDir ensures the folder exists (fileName has no path separators here,
// so its parent is always the folder itself). configdir.CreateParentDir created it
// with 0755; preserved here.
func (f *cacheFolder) CreateParentDir(string) error {
	return os.MkdirAll(f.path, 0o755)
}

// resolveCacheFolder returns the folder that stores the redeemed-codes cache.
// It prefers a local `codes/` directory in the working directory when one exists —
// the same path the Docker image mounts its volume onto — so a native run from the
// project directory shares the cache instead of writing to the OS config dir. When
// no local `codes/` exists it falls back to the per-user config dir
// (os.UserConfigDir()/bl3auto/bl3auto — the same path configdir.New("bl3auto",
// "bl3auto") resolved to on Linux, macOS, and Windows, so existing caches are found
// without migration). Overridable in tests.
var resolveCacheFolder = func() *cacheFolder {
	if fi, err := os.Stat("codes"); err == nil && fi.IsDir() {
		return &cacheFolder{path: "codes"}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return &cacheFolder{path: filepath.Join(configDir, "bl3auto", "bl3auto")}
}

// withBackoff runs op. When retry is true and op reports ErrRateLimited, it
// retries with exponential backoff and returns stop=true if the limit persists
// past rateLimitRetries (the caller should then halt the run). When retry is
// false (small/non-bulk runs) a rate-limit error is returned as-is. The backoff
// sleep aborts promptly (stop=true) if ctx is cancelled.
func withBackoff(ctx context.Context, retry bool, op func() error) (err error, stop bool) {
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
		// Honour a server-specified Retry-After when present, capped at the ceiling;
		// otherwise use the exponential backoff.
		sleep := wait
		var rle *bl3.RateLimitError
		if errors.As(err, &rle) && rle.RetryAfter > 0 {
			if sleep = rle.RetryAfter; sleep > rateLimitMaxWait {
				sleep = rateLimitMaxWait
			}
		}
		fmt.Printf("Rate limited by SHiFT; backing off %s (retry %d/%d) . . .\n", sleep, attempt, rateLimitRetries)
		if !sleepCtx(ctx, sleep) {
			return err, true
		}
		if wait *= 2; wait > rateLimitMaxWait {
			wait = rateLimitMaxWait
		}
	}
}

// gross but effective for now
const version = "2.3.5"

const SUCCESS = "success!"
const NOTFOUND = "not found."
const DOTDOTDOT = "' . . . . . "

var usernameHash string

// shiftOptions carries the resolved CLI options into the redemption routine.
type shiftOptions struct {
	source          string // "v1", "v2", or "" for default failover
	singleShiftCode string
	platformFilter  []string
	gameInclude     []string // only redeem these games (substring match; empty = all)
	gameSkip        []string // skip these games (substring match)
	dryrun          bool
	rampup          bool
	refresh         bool // re-query codes marked complete (pick up newly-linked platforms)
	count           int  // stop after this many successful redemptions (0 = no limit)
}

// cacheVersion is the current on-disk schema version of the redeemed-codes cache.
// Bump it when the layout changes so future migrations can branch on it.
const cacheVersion = 2

// expiredMarker is recorded in a code's cache entry when SHiFT reports the code as
// expired. Expiry is terminal and platform-independent, so a marked code is skipped
// entirely (no query, no redemption) on later runs. It is not a real service, so it
// never matches a redemption form or a --platform filter.
const expiredMarker = "expired"

// completeMarker is recorded when a code has been redeemed on every platform the
// SHiFT site offered for it (i.e. every linked platform). Such a code is skipped
// without a query on later runs to cut request volume; --refresh re-queries it to
// pick up platforms linked since. Like expiredMarker it is not a real service.
const completeMarker = "complete"

// markDone records a sentinel marker (expiredMarker/completeMarker) for a code,
// avoiding duplicates.
func markDone(m bl3.ShiftCodeMap, code, marker string) {
	if !m.Contains(code, marker) {
		m[code] = append(m[code], marker)
	}
}

// allServicesRedeemed reports whether every service the site offered for a code is
// recorded as redeemed — i.e. the code is complete for the currently-linked
// platforms. Returns false when there are no forms.
func allServicesRedeemed(m bl3.ShiftCodeMap, code string, forms []bl3.RedemptionForm) bool {
	if len(forms) == 0 {
		return false
	}
	for _, f := range forms {
		if !m.Contains(code, f.Service) {
			return false
		}
	}
	return true
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty tokens.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// matchesGame reports whether a code's game passes the include/skip filters
// (case-insensitive substring). An empty include list matches every game; a game
// matching any skip term is excluded (skip wins over include).
func matchesGame(include, skip []string, game string) bool {
	g := strings.ToLower(game)
	for _, s := range skip {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && strings.Contains(g, s) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, in := range include {
		if in = strings.ToLower(strings.TrimSpace(in)); in != "" && strings.Contains(g, in) {
			return true
		}
	}
	return false
}

// isAuthRedirect reports whether a redirect Location means our SHiFT session is
// gone (bounced to sign in) rather than a throttle. SHiFT's throttle 302 points at
// /home, so /home is deliberately excluded — only unambiguous sign-in markers count.
func isAuthRedirect(loc string) bool {
	if loc == "" {
		return false
	}
	l := strings.ToLower(loc)
	return strings.Contains(l, "/login") ||
		strings.Contains(l, "/sessions") ||
		strings.Contains(l, "redirect_to=false")
}

// accountPlatforms returns the sorted set of real services (platforms) seen in the
// cache — i.e. the platforms this account has redeemed on. Sentinel markers are
// excluded.
func accountPlatforms(m bl3.ShiftCodeMap) []string {
	set := map[string]struct{}{}
	for _, services := range m {
		for _, s := range services {
			if s == expiredMarker || s == completeMarker {
				continue
			}
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

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
      --game <list>       Comma-separated games to redeem (substring match; default: all),
                          e.g. --game "borderlands 3,wonderlands"
      --skip-game <list>  Comma-separated games to skip (substring match),
                          e.g. --skip-game "borderlands 4"
      --config <path>     Use a local config.json instead of the published remote config
      --dryrun            Discover and match codes but do not redeem (no side effects)
      --rampup            Cautious mode for a first run or after a long gap: paces
                          requests, backs off after 5 consecutive non-200 responses,
                          and stops cleanly after 20 (likely rate-limit/shadowban)
      --count <n>         Stop and save after n successful redemptions (0 = no limit)
      --refresh           Re-query codes already redeemed on every linked platform
                          (use after linking a new platform on your SHiFT account)
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

  # Skip a game you don't own yet (substring match, case-insensitive)
  bl3auto -e you@example.com -p 'secret' --skip-game 'borderlands 4'

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
func readRedeemedCache(folder *cacheFolder, configFilename string) (bl3.ShiftCodeMap, time.Time, bool) {
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
func writeRedeemedCache(folder *cacheFolder, configFilename string, codes bl3.ShiftCodeMap, lastRun time.Time) error {
	if folder == nil {
		return nil
	}
	cache := redeemedCache{Version: cacheVersion, LastRun: lastRun, Codes: codes}
	data, err := json.MarshalIndent(&cache, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteCache(folder, configFilename, data)
}

// atomicWriteCache writes data to fileName inside folder atomically: it writes a temp
// file in the same directory, flushes it to disk, then renames it over the destination.
// A POSIX rename is atomic, so a crash mid-write (kill -9, a full disk, power loss) can
// never truncate or corrupt the existing cache — a reader always sees either the old
// complete file or the new one. A normal Ctrl-C is caught and the run finishes this
// write before exiting, so it is unaffected.
func atomicWriteCache(folder *cacheFolder, fileName string, data []byte) (err error) {
	if err = folder.CreateParentDir(fileName); err != nil {
		return err
	}
	dir := folder.path
	dest := filepath.Join(dir, fileName)

	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Clean up the temp file if anything below fails before the rename completes.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// configdir.WriteFile used 0644; preserve that (CreateTemp makes the file 0600).
	if err = os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func loadRedeemedCodes(folder *cacheFolder, configFilename string) (bl3.ShiftCodeMap, time.Time, bool) {
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

// doShift runs the redemption loop and returns false only when it could not
// even start (the initial code-fetch failed); true otherwise, including a
// clean interrupt or a run that redeemed nothing.
func doShift(ctx context.Context, client *bl3.Bl3Client, opts shiftOptions) bool {
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

		// Pre-flight: show which platforms this account can redeem on (derived from
		// the cache) so the user can confirm their account is linked as expected.
		if plats := accountPlatforms(redeemedCodes); len(plats) > 0 {
			fmt.Println("Account can redeem on: " + strings.Join(plats, ", ") +
				" (from cache; link more at shift.gearboxsoftware.com to add platforms)")
		}

		label := "Getting new SHIFT codes"
		if opts.source != "" {
			label += " (" + opts.source + ")"
		}
		fmt.Print(label + " . . . . . ")
		list, err := client.GetShiftCodes(ctx, opts.source)
		if err != nil {
			printError(err)
			return false
		}
		fmt.Println(SUCCESS)

		// Filter the candidate list by game before querying anything — this is the
		// one filter that cuts code-query volume (codes for unwanted games are never
		// queried). --refresh ignores the filters for a full re-scan.
		if opts.refresh && (len(opts.gameInclude) > 0 || len(opts.gameSkip) > 0) {
			fmt.Println("WARNING: --refresh re-queries every code regardless of --game/--skip-game.")
		}
		if !opts.refresh && (len(opts.gameInclude) > 0 || len(opts.gameSkip) > 0) {
			kept := make([]bl3.ShiftCode, 0, len(list))
			for _, sc := range list {
				if matchesGame(opts.gameInclude, opts.gameSkip, sc.Game) {
					kept = append(kept, sc)
				}
			}
			fmt.Printf("Game filter: %d of %d codes match.\n", len(kept), len(list))
			list = kept
		}
		codes = list
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
	sessionExpired := false
	reachedCount := false
	interrupted := false
	consecutive := 0  // consecutive non-200 code-query responses (see --rampup)
	cleanStreak := 0  // consecutive clean (200) code queries (drives adaptive speed-up)
	successCount := 0 // new successful redemptions this run (see --count)
	gaveUp := 0       // codes dropped after exhausting their retries (see maxRetryAttempts)

	// Process codes from a FIFO work queue rather than a single pass: a code throttled
	// with a 302 is re-appended to the back (up to maxRetryAttempts times) so it is
	// retried at the end of the run, after every not-yet-tried code, through this same
	// loop body (and thus the same backoff/throttle/stop logic).
	type queuedCode struct {
		sc       bl3.ShiftCode
		attempts int // retries already performed (times re-queued)
	}
	queue := make([]queuedCode, len(codes))
	for i, sc := range codes {
		queue[i] = queuedCode{sc: sc}
	}

codeLoop:
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		sc := item.sc
		code := sc.Code

		// Bail out cleanly between codes if the user interrupted (Ctrl-C); the
		// end-of-run cache write below still saves progress.
		if ctx.Err() != nil {
			interrupted = true
			break codeLoop
		}

		// Skip codes we've already fully resolved, without spending a query:
		// expired (terminal), or redeemed on every linked platform (complete).
		// --refresh re-queries complete codes to detect newly-linked platforms;
		// expired codes stay skipped regardless (they can never be redeemed again).
		if redeemedCodes.Contains(code, expiredMarker) {
			continue
		}
		if !opts.refresh && redeemedCodes.Contains(code, completeMarker) {
			continue
		}

		if client.Verbose && sc.Game != "" {
			_, _ = fmt.Fprintf(os.Stderr, "[verbose] %s — game: %s\n", code, sc.Game)
		}

		var forms []bl3.RedemptionForm
		var reason string
		err, stop := withBackoff(ctx, bulk, func() error {
			var e error
			forms, reason, e = client.GetCodeRedemptionForms(ctx, code)
			return e
		})
		if stop {
			if ctx.Err() != nil {
				interrupted = true
				break codeLoop
			}
			rateLimited = true
			break codeLoop
		}
		// A non-200 code-query (commonly a 302) is SHiFT throttling us. In rampup we
		// count these: back off after a few in a row, and stop cleanly once it's
		// clearly a shadowban. A clean (200) response resets the counter.
		var statusErr *bl3.CodeQueryStatusError
		if errors.As(err, &statusErr) {
			// A redirect to sign in means our session is gone, not a throttle:
			// backing off won't help, so stop and tell the user to re-run.
			if isAuthRedirect(statusErr.Location) {
				sessionExpired = true
				break codeLoop
			}
			consecutive++
			cleanStreak = 0
			if bulk {
				// AIMD: ease off the pace so the run self-tunes to a sustainable rate.
				client.Slowdown(throttleSlowFactor, throttleCeil)
				if client.Verbose {
					_, _ = fmt.Fprintf(os.Stderr, "[verbose] non-200 query; throttle now %s\n", client.CurrentInterval())
				}
			}
			// Re-queue the code to retry at the end of the run, unless it has exhausted
			// its retries (or this isn't a bulk run). Retries still hit the backoff/stop
			// handling below.
			if bulk && item.attempts < maxRetryAttempts {
				item.attempts++
				queue = append(queue, item)
				fmt.Printf("Skipping '%s' for now (%s); will retry (attempt %d/%d).\n",
					code, err.Error(), item.attempts, maxRetryAttempts)
			} else {
				if bulk {
					gaveUp++
				}
				fmt.Println("Skipping '" + code + "': " + err.Error())
			}
			if bulk && consecutive >= stopAfter {
				stoppedShadowban = true
				break codeLoop
			}
			if bulk && consecutive >= backoffAfter {
				// Prefer the server's Retry-After when it sent one.
				wait := statusErr.RetryAfter
				if wait <= 0 {
					wait = consecutiveBackoff(consecutive)
				}
				fmt.Printf("         %d non-200 responses in a row; backing off %s . . .\n", consecutive, wait)
				if !sleepCtx(ctx, wait) {
					interrupted = true
					break codeLoop
				}
			}
			continue
		}
		var bodyErr *bl3.BodyReadError
		if errors.As(err, &bodyErr) {
			// A read failure is a transient network blip (a dropped/truncated
			// connection), not a SHiFT throttle or shadowban signal: it must not
			// affect the consecutive-non-200 counter, the adaptive throttle, or
			// the stop-after brake — just retry the code like a throttled one.
			if bulk && item.attempts < maxRetryAttempts {
				item.attempts++
				queue = append(queue, item)
				fmt.Printf("Skipping '%s' for now (%s); will retry (attempt %d/%d).\n",
					code, err.Error(), item.attempts, maxRetryAttempts)
			} else {
				if bulk {
					gaveUp++
				}
				fmt.Println("Skipping '" + code + "': " + err.Error())
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				interrupted = true
				break codeLoop
			}
			fmt.Println("Skipping '" + code + "': " + err.Error())
			continue
		}
		consecutive = 0
		if bulk {
			// AIMD: a clean streak means we can cautiously speed back up.
			cleanStreak++
			if cleanStreak >= throttleSpeedupAfter {
				client.Speedup(throttleSpeedup)
				if client.Verbose {
					_, _ = fmt.Fprintf(os.Stderr, "[verbose] clean streak; throttle now %s\n", client.CurrentInterval())
				}
				cleanStreak = 0
			}
		}
		if len(forms) == 0 {
			fmt.Println("'" + code + "': " + summarize(reason))
			// An expired code is terminal: record it so it is never re-queried.
			if strings.Contains(strings.ToLower(reason), "expired") {
				markDone(redeemedCodes, code, expiredMarker)
			}
			continue
		}

		// One redemption per form: the SHiFT site returns a form for each platform
		// linked to the account, so a code is redeemed once per linked platform
		// (--platform narrows this set).
	formLoop:
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
			rerr, stop := withBackoff(ctx, bulk, func() error { return client.RedeemForm(ctx, form) })
			if stop {
				if ctx.Err() != nil {
					fmt.Println("interrupted.")
					interrupted = true
					break codeLoop
				}
				fmt.Println("rate limited.")
				rateLimited = true
				break codeLoop
			}
			if rerr != nil {
				if ctx.Err() != nil {
					interrupted = true
					break codeLoop
				}
				fmt.Println(rerr)
				low := strings.ToLower(rerr.Error())
				switch {
				case strings.Contains(low, "expired"):
					// Expiry is global and terminal: mark the whole code and stop
					// trying its other platforms.
					markDone(redeemedCodes, code, expiredMarker)
					break formLoop
				case strings.Contains(low, "already"):
					redeemedCodes[code] = append(redeemedCodes[code], form.Service)
				}
			} else {
				redeemedCodes[code] = append(redeemedCodes[code], form.Service)
				fmt.Println(SUCCESS)
				successCount++
				// Checkpoint periodically so a hard crash (kill -9) loses at most
				// checkpointEvery redemptions rather than the whole run.
				if !opts.dryrun && checkpointEvery > 0 && successCount%checkpointEvery == 0 {
					if err := writeRedeemedCache(cacheFolder, configFilename, redeemedCodes, time.Now()); err != nil {
						printError(err)
					}
				}
				if opts.count > 0 && successCount >= opts.count {
					reachedCount = true
					break codeLoop
				}
			}
		}

		// If the code is now redeemed on every platform the site offered (and we
		// didn't filter any out with --platform), mark it complete so later runs
		// skip the query entirely. Dry runs don't redeem, so they never complete.
		if !opts.dryrun && allServicesRedeemed(redeemedCodes, code, forms) {
			markDone(redeemedCodes, code, completeMarker)
		}
	}

	switch {
	case interrupted:
		fmt.Println("Interrupted. Progress saved; re-run to continue.")
	case reachedCount:
		fmt.Printf("Reached the --count limit of %d successful redemption(s). Progress saved; re-run to continue.\n", opts.count)
	case sessionExpired:
		fmt.Println("Your SHiFT session expired or you're not signed in. Progress saved; re-run to sign in again.")
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

	if gaveUp > 0 {
		fmt.Printf("%d code(s) still throttled after %d retries; re-run later to try them again.\n", gaveUp, maxRetryAttempts)
	}

	// On any non-dryrun run, rewrite the cache: this persists newly-redeemed codes,
	// bumps lastRun (powering the stale-run warning), and upgrades an old bare-map
	// file to the current versioned format.
	if !opts.dryrun {
		if err := writeRedeemedCache(cacheFolder, configFilename, redeemedCodes, time.Now()); err != nil {
			printError(err)
		}
	}
	return true
}

// consecutiveBackoff returns the escalating sleep for the Nth consecutive non-200
// code query on a bulk run, capped at backoffMax.
func consecutiveBackoff(consecutive int) time.Duration {
	shift := consecutive - backoffAfter
	if shift < 0 {
		shift = 0
	}
	wait := backoffBase << uint(shift)
	if wait > backoffMax || wait <= 0 {
		wait = backoffMax
	}
	return wait
}

func main() {
	os.Exit(run())
}

// run implements the CLI entry point and returns the process exit code: 0 on
// success or a clean interrupt, 1 on a setup/login/code-fetch failure. The
// mutually-exclusive-flags check exits directly with 2 (before any cleanup is
// registered).
func run() int {
	var (
		username        string
		password        string
		singleShiftCode string
		allowInactive   bool
		useV1           bool
		useV2           bool
		platform        string
		game            string
		skipGame        string
		configPath      string
		dryrun          bool
		verbose         bool
		rampup          bool
		refresh         bool
		migrate         bool
		count           int
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
	flag.StringVar(&game, "game", "", "Comma-separated games to redeem (substring match; default: all)")
	flag.StringVar(&skipGame, "skip-game", "", "Comma-separated games to skip (substring match)")
	flag.StringVar(&configPath, "config", "", "Use a local config.json instead of the published remote config")
	flag.BoolVar(&dryrun, "dryrun", false, "Discover and match codes but do not redeem")
	flag.BoolVar(&rampup, "rampup", false, "Cautious mode for a first run / long gap: pace requests and stop cleanly if throttled")
	flag.IntVar(&count, "count", 0, "Stop and save after this many successful redemptions (0 = no limit)")
	flag.BoolVar(&refresh, "refresh", false, "Re-query codes already redeemed on every linked platform (to pick up newly-linked platforms)")
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

	platformFilter := splitCSV(platform)
	gameInclude := splitCSV(game)
	gameSkip := splitCSV(skipGame)

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
		return 0
	}

	if password == "" {
		fmt.Print("Enter password        : ")
		if term.IsTerminal(int(os.Stdin.Fd())) {
			pw, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				fmt.Printf("error: failed to read password: %v\n", err)
				os.Exit(1)
			}
			password = string(pw)
		} else {
			reader := bufio.NewReader(os.Stdin)
			line, _, _ := reader.ReadLine()
			password = string(line)
		}
	}

	// A Ctrl-C (or SIGTERM) during setup, login, or a long throttled run should
	// stop cleanly and save progress rather than discard the whole run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Print("Setting up . . . . . ")
	client, err := bl3.NewBl3Client(ctx, configPath)
	if err != nil {
		printError(err)
		return 1
	}
	client.Verbose = verbose
	client.Config.Shift.AllowInactive = allowInactive
	fmt.Println(SUCCESS)

	if client.Config.Version != version {
		fmt.Println("Your version (" + version + ") is out of date. Please consider downloading the latest version (" + client.Config.Version + ") at https://github.com/jauderho/bl3auto/releases/latest")
	}

	fmt.Print("Logging in as '" + username + DOTDOTDOT)
	if err = client.Login(ctx, username, password); err != nil {
		printError(err)
		return 1
	}
	fmt.Println(SUCCESS)

	ok := doShift(ctx, client, shiftOptions{
		source:          source,
		singleShiftCode: singleShiftCode,
		platformFilter:  platformFilter,
		gameInclude:     gameInclude,
		gameSkip:        gameSkip,
		dryrun:          dryrun,
		rampup:          rampup,
		refresh:         refresh,
		count:           count,
	})
	if !ok {
		return 1
	}

	// On Ctrl-C/SIGTERM, doShift has already stopped cleanly and saved progress; exit
	// promptly rather than making the user sit through the countdown.
	if ctx.Err() != nil {
		fmt.Println("Interrupted; exiting.")
		return 0
	}

	exit()
	return 0
}
