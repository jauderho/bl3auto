FROM ghcr.io/jauderho/golang:1.26.4-alpine3.23@sha256:d893e6d165d70d6da631f70850f645edeaf426b60e7394d41dac025d71157390 AS build

COPY . /go/src/github.com/jauderho/bl3auto
WORKDIR /go/src/github.com/jauderho/bl3auto

ENV GO111MODULE=on

RUN apk update \
	&& apk add --no-cache git \
	&& go mod download \
	&& go mod verify \
	&& go build -v -trimpath -ldflags="-s -w" cmd/bl3auto.go


# ----------------------------------------------------------------------------


#FROM scratch
FROM ghcr.io/jauderho/alpine:3.24.1@sha256:f310f8a4fad676899731a97c5eaf004c9cb9e822018a2ad7d23c3aeda3a7d773

LABEL org.opencontainers.image.authors="Jauder Ho <jauderho@users.noreply.github.com>"
LABEL org.opencontainers.image.url="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.documentation="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.source="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.title="jauderho/bl3auto"
LABEL org.opencontainers.image.description="Borderlands Auto SHiFT Code Redemption System"

COPY --from=build /go/src/github.com/jauderho/bl3auto/bl3auto /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/bl3auto"]
