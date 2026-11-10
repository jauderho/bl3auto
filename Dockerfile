FROM ghcr.io/jauderho/golang:1.27.3-alpine3.24@sha256:df374877c88e071326eb78b141be10ba6a57c4dd41b3ddf5988dc68152276817 AS build

COPY . /go/src/github.com/jauderho/bl3auto
WORKDIR /go/src/github.com/jauderho/bl3auto

ENV GO111MODULE=on
# Static binary: the scratch runtime image has no libc.
ENV CGO_ENABLED=0

RUN apk update \
	&& apk add --no-cache git ca-certificates \
	&& go mod download \
	&& go mod verify \
	&& go build -v -trimpath -ldflags="-s -w" cmd/bl3auto.go


# ----------------------------------------------------------------------------


FROM scratch

LABEL org.opencontainers.image.authors="Jauder Ho <jauderho@users.noreply.github.com>"
LABEL org.opencontainers.image.url="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.documentation="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.source="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.title="jauderho/bl3auto"
LABEL org.opencontainers.image.description="Borderlands Auto SHiFT Code Redemption System"

# CA roots for HTTPS to SHiFT and GitHub raw (scratch ships none).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /go/src/github.com/jauderho/bl3auto/bl3auto /usr/local/bin/

# scratch has no /etc/passwd, so pin HOME: os.UserConfigDir() must keep
# resolving to /root/.config, where the codes volume is mounted.
ENV HOME=/root

ENTRYPOINT ["/usr/local/bin/bl3auto"]
