// promptshipper-mcp — an MCP server that gives an AI agent a two-way channel to
// the user's devices through the promptshipper clip server.
//
// The agent can read what the user captured on their phone and desktop
// (recent_clips) and hand text back to the phone (push_clip).
// It is a thin client of server/clip's HTTP API and stores nothing itself.
//
// Transport: stdio only. A streamable-HTTP listener would be a second network
// surface exposing the user's clipboard, and it would need an auth scheme of
// its own; every client this targets (Claude Code, Claude Desktop, other local
// MCP hosts) launches stdio servers. See README.md.
//
// 🔴 THE CLIP TOKEN NEVER LEAVES THIS PROCESS EXCEPT IN THE Authorization
// HEADER. Both output streams are wrapped in a redactingWriter (redact.go), so
// a token that reaches a tool result, an error or a log line by any path is
// replaced before it is written. TestTheTokenNeverAppearsInAnyOutput pins it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildVersion is stamped with -ldflags "-X main.buildVersion=…".
var buildVersion = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the process exits, so a test can drive the real wiring.
// Exit codes: 0 clean shutdown, 1 the session failed, 2 configuration refused.
func run(args []string, getenv func(string) string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("promptshipper-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pairing := fs.String("pairing", "", "path to a pairing payload JSON file (docs/pairing.md, v1); overrides $"+envPairingFile)
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *version {
		fmt.Fprintln(stdout, buildVersion)
		return 0
	}

	cfg, err := loadConfig(getenv, *pairing)
	if err != nil {
		// Printed before the redacting writer exists; loadConfig's errors are
		// pinned never to contain the token (TestConfigErrorsNeverEchoTheToken).
		fmt.Fprintf(stderr, "promptshipper-mcp: %v\n", err)
		return 2
	}

	errw := newRedactingWriter(stderr, cfg.Token)
	defer errw.Close()
	outw := newRedactingWriter(stdout, cfg.Token)
	logger := slog.New(slog.NewTextHandler(errw, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("promptshipper-mcp starting", "version", buildVersion, "config", cfg.String())

	srv := newServer(newClipClient(cfg), &mcp.ServerOptions{Logger: logger})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = srv.Run(ctx, &mcp.IOTransport{Reader: stdin, Writer: outw})
	if err != nil && ctx.Err() == nil {
		logger.Error("session ended", "err", err)
		return 1
	}
	return 0
}
