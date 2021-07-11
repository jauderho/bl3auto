FROM golang:1.16.5-alpine3.13

COPY . /go/src/github.com/jauderho/bl3auto
WORKDIR /go/src/github.com/jauderho/bl3auto

ENV GO111MODULE=on

RUN apk update && apk add git
RUN go mod download && go mod verify

#CMD go run cmd/main.go
#CMD go run -v -ldflags="-s -w" cmd/main.go

RUN go build -v -ldflags="-s -w" cmd/bl3auto.go
CMD ./bl3auto
