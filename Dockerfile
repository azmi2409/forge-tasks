# forge-tasks API — multi-stage build, static binary, no cgo.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-tasks-api ./cmd/forge-tasks-api

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /forge-tasks-api /usr/local/bin/forge-tasks-api
EXPOSE 8080
ENTRYPOINT ["forge-tasks-api"]
