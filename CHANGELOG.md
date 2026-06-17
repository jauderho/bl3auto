# ChangeLog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com), and this
project adheres to [Semantic Versioning](https://semver.org/).

## v2.3.0 - 2026-06-16
### Changed
* Fixed authentication after Gearbox retired the `api.2k.com` Borderlands API.
  Login and redemption now use the Gearbox SHiFT website
  (`shift.gearboxsoftware.com`): CSRF login, then per-service redemption forms
  with async status polling.
### Added
* Borderlands 4 SHiFT code support.
* Two code-list formats with dedicated parsers: v2
  (ugoogalizer/mentalmars `shiftcodes.json`, default) and v1 (orcicorn
  `index.json`). Redemption uses v2 and falls back to v1 if v2 is unavailable.
* New flags: `--v1`, `--v2`, `--platform`, `--config`, `--dryrun`, `-v/--verbose`,
  and a documented `--help`.
* `--allow-inactive` now includes codes flagged as expired in the v2 source.
* Rate-limit handling for bulk redemption: requests are paced and the tool backs
  off (and stops cleanly, saving progress) on HTTP 429/503 instead of hammering.
* The runtime config is now embedded in the binary as a fallback, so a freshly
  compiled binary works without `--config` or network access. The published
  remote config is still preferred when reachable and compatible (for hot-fixes);
  an unreachable or older-schema remote config falls back to the embedded one.
* `--rampup`: a cautious mode for a first run or after a long gap. It paces requests
  much more conservatively, backs off after 5 consecutive non-200 code-query
  responses (SHiFT soft-rate-limits with 302s), and stops cleanly after 20 in a row
  (likely shadowban) with progress saved.
* First-run / long-gap warning: when no redeemed-codes cache exists or the last run
  was over ~6 months ago, the tool recommends re-running with `--rampup`.
* The redeemed-codes cache is now a versioned format (`{version, lastRun, codes}`).
  Old bare-map files are still read, and any normal run upgrades the file in place
  and stamps `lastRun`.
* `--migrate`: a standalone, login-free command to upgrade the redeemed-codes cache
  file in place to the current version (`-e` selects the per-account cache). Useful
  for explicitly converting an old file without a redemption run.
* `--count <n>`: stop and save after `n` successful redemptions (0 = no limit).
  Handy with `--rampup` to cap how much a single run attempts.
* Expired codes are now recorded in the cache (terminal state) and skipped entirely
  on later runs — no repeated query or redemption attempt.
* The cache now lives in a local `codes/` directory when one exists in the working
  directory (the same path the Docker image mounts), so a native run from the project
  directory shares the cache instead of using the per-user OS config dir.

## v2.2.13 - 2022-01-18
### Added
* Use goreleaser to build & release
* Add cosign and SBOM support. Credit to @shibumi and https://github.com/in-toto/in-toto-golang/pull/128/files

## v2.2 - 2021-07-10
### Changed
* Removed all VIP code
* Only support SHiFT codes going forward
* Fixed broken login process
* Normalized to consistent naming of bl3auto
* Try to redeem all SHiFT codes instead of just Borderlands 3

## v2.1.0 - 2019-09-18
### Added
* GitHub site - https://matt1484.github.io/bl3_auto_vip/
* Go modules ([#1])
* GitHub Actions CI pipeline ([#6])
* Changelog
* SHiFT code support ([#9])
* Ability to redeem activities ([#10])
* Config for future/updates

### Changed
* Improve README

## v2.0.0 - 2019-09-11
### Changed
* Rewrote all code in go to add future mobile support (also more maintainable
and smaller executable)

## v1.2.1 - 2019-08-28
### Fixed
* Fixed bug where tables in comments would count as codes

### Added
* Password masking

## v1.2.0 - 2019-08-27
### Added
* Timer so it does not immediately close when done
* Support for codes with multiple types

### Fixed
* Bad logging around/error handling involving code type setup

## v1.1.0 - 2019-08-25
### Added
* Support for command-line args (email and password)

### Changed
* Now uses REST endpoints and JSON parsing rather than headless browser
* Utilize .net core 3.0

### Fixed
* Timeout issues and added 

## v1.0.0 - 2019-08-22
* Initial release
