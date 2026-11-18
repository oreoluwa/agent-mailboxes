# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET=./cmd/emaild
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/app ${TARGET}

FROM alpine:3.22 AS runtime
WORKDIR /app

RUN apk add --no-cache ca-certificates poppler-utils tzdata

COPY --from=builder /out/app /usr/local/bin/app
COPY LICENSE THIRD_PARTY_NOTICES.md ./
COPY third_party ./third_party
COPY migrations ./migrations

CMD ["/usr/local/bin/app"]
