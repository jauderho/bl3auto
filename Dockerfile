FROM ghcr.io/jauderho/golang:1.25.4-alpine3.22@sha256:d05e734fbc49e799f7be117d5e7329831efbb93f34b39b8cf1c59b33dbdd15e8 AS build

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
FROM ghcr.io/jauderho/alpine:3.22.2@sha256:973657c02945c52bf2ab56231eba7f2163d960e14e3ec39e51c61b255b27128b

LABEL org.opencontainers.image.authors="Jauder Ho <jauderho@users.noreply.github.com>"
LABEL org.opencontainers.image.url="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.documentation="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.source="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.title="jauderho/bl3auto"
LABEL org.opencontainers.image.description="Borderlands Auto SHiFT Code Redemption System"

COPY --from=build /go/src/github.com/jauderho/bl3auto/bl3auto /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/bl3auto"]
