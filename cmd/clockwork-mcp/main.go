// Command clockwork-mcp is an MCP (Model Context Protocol) server that lets an
// AI agent manage one Clockwork application's tasks over stdio.
//
// It is configured through the environment:
//
//	CLOCKWORK_URL      Clockwork's base URL (default http://localhost:8080)
//	CLOCKWORK_API_KEY  an application API key (cwk_...); the admin token is refused
//
// Register it with an MCP client, for example Claude Code:
//
//	claude mcp add clockwork --env CLOCKWORK_API_KEY=cwk_... -- clockwork-mcp
//
// Stdout carries the protocol; diagnostics go to stderr.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/farhan-hilmy/clockwork/internal/mcpserver"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "clockwork-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	baseURL := os.Getenv("CLOCKWORK_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	server, err := mcpserver.New(checkCtx, mcpserver.Config{
		URL:     baseURL,
		APIKey:  os.Getenv("CLOCKWORK_API_KEY"),
		Version: version,
	})
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}
