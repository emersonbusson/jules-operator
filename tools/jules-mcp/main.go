package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

func main() {
	apiKey := os.Getenv("JULES_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "JULES_API_KEY não configurada")
		os.Exit(2)
	}

	baseURL := os.Getenv("JULES_API_BASE_URL")
	client, err := newJulesClient(apiKey, baseURL, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if len(os.Args) > 1 && os.Args[1] == "batch" {
		if err := runBatch(context.Background(), client, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	mcpServer := newMCPServer(client, os.Getenv("JULES_ALLOWED_SOURCE"), logger)
	if err := server.NewStdioServer(mcpServer).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
