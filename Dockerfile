FROM golang:1.21.1-alpine3.18@sha256:96634e55b363cb93d39f78fb18aa64abc7f96d372c176660d7b8b6118939d97b AS build

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
FROM alpine:3.18.3@sha256:7144f7bab3d4c2648d7e59409f15ec52a18006a128c733fcff20d3a4a54ba44a

LABEL org.opencontainers.image.authors="Jauder Ho <jauderho@users.noreply.github.com>"
LABEL org.opencontainers.image.url="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.documentation="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.source="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.title="jauderho/bl3auto"
LABEL org.opencontainers.image.description="Borderlands Auto SHiFT Code Redemption System"

COPY --from=build /go/src/github.com/jauderho/bl3auto/bl3auto /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/bl3auto"]
