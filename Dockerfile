# forge-tasks — multi-stage build, static binaries, no cgo.
# Two targets: `api` (REST + SSE + web UI) and `mcp` (stdio MCP server).
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-tasks-api ./cmd/forge-tasks-api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forge-tasks-mcp ./cmd/forge-tasks-mcp

FROM alpine:3.21 AS api
RUN apk add --no-cache ca-certificates wget
COPY --from=builder /forge-tasks-api /usr/local/bin/forge-tasks-api
EXPOSE 8080
ENTRYPOINT ["forge-tasks-api"]

FROM alpine:3.21 AS mcp
RUN apk add --no-cache ca-certificates
COPY --from=builder /forge-tasks-mcp /usr/local/bin/forge-tasks-mcp
# stdio transport: no ports, keep stdin open.
ENTRYPOINT ["forge-tasks-mcp"]
