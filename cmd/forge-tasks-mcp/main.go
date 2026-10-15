// Command forge-tasks-mcp runs the forge-tasks MCP server over stdio: the
// primary, designed-first interface for AI agents.
//
// Configure your MCP client with:
//
//	{
//	  "mcpServers": {
//	    "forge-tasks": {
//	      "command": "forge-tasks-mcp",
//	      "env": { "DATABASE_URL": "postgres://... (optional)" }
//	    }
//	  }
//	}
//
// Without DATABASE_URL it serves an in-memory store (great for trying the
// tool surface; not for real work).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	forgemcp "github.com/azmi2409/forge-tasks/mcp"
	"github.com/azmi2409/forge-tasks/service"
	"github.com/azmi2409/forge-tasks/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if err := store.ApplyMigration(ctx, url); err != nil {
			logger.Error("migration failed", "err", err)
			os.Exit(1)
		}
		ps, err := store.NewPostgresStore(ctx, url)
		if err != nil {
			logger.Error("postgres connect failed", "err", err)
			os.Exit(1)
		}
		defer ps.Close()
		st = ps
		logger.Info("mcp serving postgres store")
	} else {
		st = store.NewMemoryStore()
		logger.Warn("DATABASE_URL not set; serving an ephemeral in-memory store")
	}

	server := forgemcp.NewServer(service.New(st, nil))
	logger.Info("mcp server starting on stdio")
	if err := server.Run(ctx, sdk.NewStdioTransport()); err != nil {
		logger.Error("mcp server failed", "err", err)
		os.Exit(1)
	}
}
