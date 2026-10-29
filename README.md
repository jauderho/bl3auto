# FORKED : BL3 Auto SHiFT

[![Go Report Card](https://goreportcard.com/badge/github.com/jauderho/bl3auto)](https://goreportcard.com/report/github.com/jauderho/bl3auto)
[![GitHub Super-Linter](https://github.com/jauderho/bl3auto/workflows/Lint%20Code%20Base/badge.svg)](https://github.com/jauderho/bl3auto/actions/workflows/linter.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/jauderho/bl3auto/badge)](https://securityscorecards.dev/viewer/?uri=github.com/jauderho/bl3auto)

Cross platform Go app for automatically redeeming SHiFT codes
for all Borderlands and Wonderlands games, including **Borderlands 4**.

This was forked from matt1484's [repo](https://github.com/matt1484/bl3_auto_vip) as it appears to be no longer maintained. Since VIP is discontinued, all VIP code has been removed. This will only redeem SHiFT codes going forward.

Authentication uses the Gearbox SHiFT website (`shift.gearboxsoftware.com`) directly,
following the login + redemption flow (the old `api.2k.com` Borderlands API is gone).


## Getting Started

1. Make a SHiFT account at [shift.gearboxsoftware.com](https://shift.gearboxsoftware.com)
   and link the platforms you play on (Steam, Epic, PSN, Xbox, ...).
2. Install bl3auto (see [Installing](#installing)): Docker Compose, Docker, Homebrew, or a
   prebuilt binary.
3. Give it your SHiFT email and password (see [Credentials](#credentials)). They are only
   sent to `shift.gearboxsoftware.com` to sign in.
4. Run it. The first time, add `--rampup` so SHiFT doesn't rate-limit a large batch.
5. Run it again whenever new codes come out, or schedule it (see
   [Running on a schedule](#running-on-a-schedule)).

Run it with `--help` to view command line args that are supported.

### Credentials

bl3auto takes your SHiFT email and password, in this order of precedence:

1. The `-e`/`--email` and `-p`/`--password` flags.
2. The `BL3_EMAIL` and `BL3_PASSWORD` environment variables.
3. Otherwise it prompts for them (the password is not echoed).

Prefer the environment variables (or the prompt) over `-p`: command-line arguments are
visible to other users on the machine in the process list.

```sh
export BL3_EMAIL='me@example.com'
export BL3_PASSWORD='secret'
bl3auto
```

### Command line flags

| Flag | Description |
|---|---|
| `-e`, `--email` | SHiFT account email (default: the `BL3_EMAIL` environment variable, else prompted) |
| `-p`, `--password` | SHiFT account password (default: the `BL3_PASSWORD` environment variable, else prompted). Prefer the variable: `-p` is visible to other users in the process list |
| `--shift-code <code>` | Redeem a single SHiFT code instead of the full list |
| `--allow-inactive` | Attempt to redeem inactive SHiFT codes too |
| `--v1` | Force the original orcicorn code source |
| `--v2` | Force the newer ugoogalizer/mentalmars code source |
| `--platform <list>` | Comma-separated services to redeem on (`steam,epic,psn,xboxlive,nintendo,stadia`); default: all offered |
| `--game <list>` | Only redeem these games (case-insensitive substring, e.g. `"borderlands 3,wonderlands"`); default: all |
| `--skip-game <list>` | Skip these games (case-insensitive substring, e.g. `"borderlands 4"`) |
| `--config <path>` | Use a local `config.json` instead of the published remote config |
| `--dryrun` | Discover and match codes but do not redeem (no side effects) |
| `--rampup` | Cautious mode for a first run or after a long gap: paces requests, backs off after 5 consecutive non-200 responses, and stops cleanly after 20 (likely rate-limit/shadowban) |
| `--count <n>` | Stop and save after `n` successful redemptions (`0` = no limit) |
| `--refresh` | Re-query codes already in the redeemed-codes cache (run after linking a new platform on your SHiFT account) |
| `--migrate` | Upgrade the redeemed-codes cache file in place to the current version and exit (no login; `-e` selects the per-account cache) |
| `-v`, `--verbose` | Verbose step-level logging to stderr |

> **First run, or first in a while?** SHiFT readily rate-limits a large redemption
> (it answers with 302s once it's throttling you). Run with `--rampup` the first time,
> or after months away, to pace requests and stop cleanly instead of getting
> shadowbanned. bl3auto reminds you when it looks like a first or long-overdue run.

On bulk runs the request pacing is **adaptive**: it automatically slows down when SHiFT
starts throttling (302s) and speeds back up after a clean streak, so it settles on the
fastest rate SHiFT tolerates without you having to tune anything. `-v` shows the interval
adjusting in real time.

#### Platforms

bl3auto redeems each code on **every platform linked to your SHiFT account** — the
site returns one redemption form per linked service, so a "universal" code is redeemed
once per linked platform (e.g. Steam *and* Epic). To cover more platforms (PSN, Xbox,
Nintendo, Stadia), link them once at
[shift.gearboxsoftware.com](https://shift.gearboxsoftware.com) and bl3auto will pick
them up automatically. Use `--platform` to narrow redemption to a subset of services.
Before a bulk run, bl3auto prints the platforms your account can redeem on (from the
cache) so you can confirm your account is linked as you expect.

#### Skipping games you don't want

SHiFT redemption isn't gated by game ownership — a code for a game you don't own still
redeems and the reward waits until you play. If you'd rather not spend requests on a
game (e.g. one you haven't bought yet), use `--skip-game "borderlands 4"` or restrict
with `--game "borderlands 3,wonderlands"`. Unlike `--platform`, the game filter drops
codes **before** they're queried, so it's the one filter that actually reduces request
volume. It's per-run (not remembered); `--refresh` ignores it for a full re-scan.

#### Code sources

bl3auto reads SHiFT codes from two sources and, by default, uses the newer (v2)
source and falls back to the original (v1) source only if v2 is unavailable:

- **v2** (default) — [ugoogalizer/autoshift-codes](https://github.com/ugoogalizer/autoshift-codes) (`shiftcodes.json`, includes Borderlands 4)
- **v1** — [jauderho/shift-codes](https://github.com/jauderho/shift-codes) (orcicorn `index.json`)

Use `--v1` or `--v2` to force a single source.

#### Redeemed-codes cache

bl3auto remembers what it has already redeemed (and which codes are expired) in a
per-account JSON file so it doesn't reattempt them. Progress is crash-safe: pressing
Ctrl-C stops the run cleanly with what's been redeemed saved, and the cache is also
checkpointed periodically during a run, so an interruption never forces you to re-query
everything (which would only trip SHiFT's rate limit faster). Any code already in the
cache — and every expired code — is skipped without even a query on later runs to cut
request volume, and codes are processed newest-first; if you link a new platform on
your SHiFT account (or a redemption on one platform failed), run with `--refresh` once
so cached codes are re-checked. If a `codes/` directory exists in
the working directory, that file is stored there — the same path the Docker image
mounts its volume onto — so a native run from the project directory shares the cache
with Docker. Otherwise it falls back to the per-user OS config directory
(e.g. `~/Library/Application Support/bl3auto/bl3auto` on macOS,
`~/.config/bl3auto/bl3auto` on Linux). Run `mkdir codes` before running natively if
you want the cache kept alongside the project. Use `--migrate` to upgrade an existing
cache file to the current format.

### Installing

Multi-platform images are published to Docker Hub
([`jauderho/bl3auto`](https://hub.docker.com/r/jauderho/bl3auto/)) and GHCR
(`ghcr.io/jauderho/bl3auto`), tagged `latest` and with each release version (e.g.
`v2.3.9`). The image's entrypoint is `bl3auto`, so anything after the image name is
passed as flags. The redeemed-codes cache lives at `/root/.config/bl3auto/bl3auto` inside
the container; mount a directory there so it survives between runs.

#### Docker Compose (preferred)

1. Install Docker with the Compose plugin.
2. In a new directory, create the cache directory: `mkdir codes`
3. Create a `.env` file with your credentials (keep it private, e.g. `chmod 600 .env`):

   ```sh
   BL3_EMAIL=me@example.com
   BL3_PASSWORD=secret
   ```

4. Create `compose.yml` (as in this repo's [compose.yml](compose.yml)):

   ```yaml
   services:
     bl3auto:
       container_name: bl3auto
       image: jauderho/bl3auto:latest
       environment:
         BL3_EMAIL: ${BL3_EMAIL}
         BL3_PASSWORD: ${BL3_PASSWORD}
       volumes:
         - './codes:/root/.config/bl3auto/bl3auto'
   ```

5. Run `docker compose up`. To pass flags, add a `command:` with them, e.g.
   `command: ["--rampup"]` for the first run.

#### Docker

```sh
mkdir -p codes
docker run -it --rm \
  -e BL3_EMAIL -e BL3_PASSWORD \
  -v "$PWD/codes:/root/.config/bl3auto/bl3auto" \
  jauderho/bl3auto:latest
```

`-e BL3_EMAIL -e BL3_PASSWORD` passes the variables from your shell; leave them out to be
prompted instead. Append flags after the image name, e.g. `jauderho/bl3auto:latest --dryrun`.

#### Homebrew (macOS)

```sh
brew install --cask jauderho/bl3auto/bl3auto
```

#### Prebuilt binaries and packages

Download an archive for your platform, or a `.deb`/`.rpm` package, from the
[releases page](https://github.com/jauderho/bl3auto/releases), unpack it and run
`bl3auto`. Every artifact is signed with [cosign](https://github.com/sigstore/cosign)
(`.bundle` files) and has an SBOM.

### Running on a schedule

New codes appear every few days. A daily cron job is plenty, e.g. with Docker Compose:

```sh
0 9 * * * cd /path/to/bl3auto && docker compose up >> bl3auto.log 2>&1
```

## FAQs

### Why does my operating system say it's an unrecognized/untrusted app?
Telling the operating system that we're a trusted source is expensive.
This is a small open source project and we don't have the funds to correctly
sign the app.

### Running the app on macOS
macOS may refuse to run the app because it is "from an unidentified developer".
To get around this, right click on the app in Finder, and while holding the `⌥ Option` key,
click `Open` in the menu. You will be prompted with a message similar to this:

>macOS cannot verify the developer of "bl3auto". Are you sure you want to open it?

Click the `Open` button and the app will run in your terminal. From that point forward
you will be able to run the app directly or from your terminal without any issues.

### Why does my antivirus flag this program?
It's a false positive. If you don't trust us, you can look at the code and
compile it yourself. That's one of the beauties of an open source project!

### It says "launch a SHiFT-enabled title first, then try again later"
SHiFT stops accepting redemptions for your account after a batch of them, until you
launch a SHiFT-enabled Borderlands or Wonderlands game (and sign in to SHiFT in it).
bl3auto stops the run when this happens and saves progress. Launch a game, then run
bl3auto again; it skips codes it has already redeemed.

### It's not working. What should I do?
File an issue here with as much detail as you can provide. We're working on
adding additional logging and a bug template to better assist with any issues.

## License
This project is licensed under the Apache-2.0 License - see the
[LICENSE](LICENSE) file for details
