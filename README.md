# FORKED : BL3 Auto SHiFT

[![Go Report Card](https://goreportcard.com/badge/github.com/jauderho/bl3auto)](https://goreportcard.com/report/github.com/jauderho/bl3auto)
[![GitHub Super-Linter](https://github.com/jauderho/bl3auto/workflows/Lint%20Code%20Base/badge.svg)](https://github.com/jauderho/bl3auto/actions/workflows/linter.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/jauderho/bl3auto/badge)](https://securityscorecards.dev/viewer/?uri=github.com/jauderho/bl3auto)

Cross platform Go app for automatically redeeming SHiFT codes
for all Borderlands and Wonderlands games.

This was forked from matt1484's [repo](https://github.com/matt1484/bl3_auto_vip) as it appears to be no longer maintained. Since VIP is discontinued, all VIP code has been removed. This will only redeem SHiFT codes going forward.


## Getting Started

1. Make a SHiFT account at [Borderlands](https://borderlands.com/)
2. Download program from above link
3. Unzip the folder
4. Run it, you will be prompted for username and password
5. Enter username and password (we only use this info to sign into borderlands)
6. Watch it do its magic
7. Repeat when more codes come out (or set up a cron job)


Run it with `--help` to view command line args that are supported.

### Installing

#### Docker
Source: https://hub.docker.com/r/jauderho/bl3auto/
1. Install Docker
2. Run `docker pull jauderho/bl3auto:latest`
3. Optional: Create `codes` subdirectory to store output from previous runs with `mkdir codes`
4. Run `docker run -it -v codes:/root/.config/bl3auto/bl3auto jauderho/bl3auto:latest`
    + The mounted volume will keep track of existing codes that have been used already

#### Docker Compose (preferred)
1. Install Docker and docker-compose
2. Create .env and put the following in the file
    + Add `BL3_EMAIL="me@myemail.com" and BL3_PASSWORD="mypassword"`
    + Replace `"me@myemail.com"` with your login email address
    + Replace `"mypassword"` with your login password
3. Use the compose.yml file below (Updated as of 10/16/2021)

```
services:
  bl3auto:
    container_name: bl3auto
    image: jauderho/bl3auto:latest
    command: ["-e", "${BL3_EMAIL}", "-p", "${BL3_PASSWORD}"]
    volumes:
      - './codes:/root/.config/bl3auto/bl3auto'

volumes:
  codes:
```

4. Optional: Create `codes` subdirectory to store output from previous runs with `mkdir codes`
    + Doing so will allow bl3auto to compare and avoid trying to redeem a previously used code
5. Run `docker-compose up`

#### Using the prebuilt releases
The binaries/executables are released
[here](https://github.com/jauderho/bl3auto/releases)

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

### It's not working. What should I do?
File an issue here with as much detail as you can provide. We're working on
adding additional logging and a bug template to better assist with any issues.

## License
This project is licensed under the Apache-2.0 License - see the
[LICENSE](LICENSE) file for details
