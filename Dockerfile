FROM golang:1.10-alpine AS builder

WORKDIR /go/src/app
COPY . .

RUN apk add --no-cache git

RUN go get -d -v ./...
RUN go install -v ./...

FROM alpine

COPY --from=builder /go/bin/app /usr/bin/app

ENTRYPOINT ["app"]

