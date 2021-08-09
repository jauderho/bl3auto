FROM golang:alpine as build

COPY . /go/src/github.com/jauderho/bl3auto
WORKDIR /go/src/github.com/jauderho/bl3auto

ENV GO111MODULE=on

RUN apk update \
	&& apk add git \
	&& go mod download \
	&& go mod verify \
	&& go build -v -ldflags="-s -w" cmd/bl3auto.go

#CMD go run cmd/main.go
#CMD go run -v -ldflags="-s -w" cmd/main.go

#RUN go build -v -ldflags="-s -w" cmd/bl3auto.go

RUN ls -lh /go/src/github.com/jauderho/bl3auto/bl3auto 

# ----------------------------------------------------------------------------

#FROM scratch
FROM alpine:3

LABEL org.opencontainers.image.authors="Jauder Ho <jauderho@users.noreply.github.com>"
LABEL org.opencontainers.image.url="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.documentation="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.source="https://github.com/jauderho/bl3auto"
LABEL org.opencontainers.image.title="jauderho/bl3auto"
LABEL org.opencontainers.image.description="Borderlands3 Auto SHiFT Code Redemption System"

COPY --from=build /go/src/github.com/jauderho/bl3auto/bl3auto /usr/local/bin/

#ENTRYPOINT ["/usr/local/bin/bl3auto"]

CMD ["bl3auto"]
