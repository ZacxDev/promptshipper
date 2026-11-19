package main

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tool set is deliberately three tools. Each one maps to exactly one
// capability the clip server has; there is no tool for DELETE /v1/clips/{id}
// (an agent deleting the user's clipboard history is a destructive action with
// no use in the read/push loop this server exists for) and none for the STT
// server (dictation reaches an agent as clips: the app captures transcripts to
// the clip server, and STT has no history of its own).

// defaultRecentLimit is smaller than the clip server's default of 100 on
// purpose: every row lands in the agent's context window, and "what did I just
// highlight" is answered by the newest handful.
const defaultRecentLimit = 20

type clipOut struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Device    string `json:"device" jsonschema:"the device that captured it (e.g. the phone's name, or mcp for text an agent pushed)"`
	CreatedAt string `json:"created_at" jsonschema:"when it was captured or last re-captured, RFC 3339 UTC"`
	BumpCount int64  `json:"bump_count" jsonschema:"how many times this exact text was captured again after the first time"`
}

func toOut(c clip) clipOut {
	return clipOut{
		ID: c.ID, Body: c.Body, Device: c.Device, BumpCount: c.BumpCount,
		CreatedAt: time.UnixMilli(c.CreatedAt).UTC().Format(time.RFC3339),
	}
}

type recentIn struct {
	Limit int    `json:"limit,omitempty" jsonschema:"how many clips to return, newest first: 1..500, default 20"`
	Query string `json:"query,omitempty" jsonschema:"optional search: every whitespace-separated word must appear in the clip as a case-insensitive substring"`
}

type recentOut struct {
	Clips []clipOut `json:"clips"`
}

type getIn struct {
	ID int64 `json:"id" jsonschema:"the clip id from recent_clips"`
}

type pushIn struct {
	Text string `json:"text" jsonschema:"the text to send to the user's devices (max 256 KiB)"`
}

type pushOut struct {
	ID      int64  `json:"id"`
	Deduped bool   `json:"deduped" jsonschema:"true when identical text was already stored and was moved to the top instead of duplicated"`
	Note    string `json:"note"`
}

// pushNote is returned with every push so the agent does not tell the user
// "it's on your phone's clipboard" — it is not. See README "Push to phone".
const pushNote = "Stored as the newest clip. It appears at the top of the clip list in the promptshipper app and in snip; " +
	"the user copies it from there. It is NOT written to the phone's system clipboard automatically."

func newServer(cl *clipClient, device string, opts *mcp.ServerOptions) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "promptshipper", Version: buildVersion}, opts)

	mcp.AddTool(s, &mcp.Tool{
		Name: "recent_clips",
		Description: "Read what the user recently highlighted, dictated or copied on their phone and desktop " +
			"(the promptshipper shared clipboard), newest first. Optionally filter with a search query.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recentIn) (*mcp.CallToolResult, recentOut, error) {
		limit := in.Limit
		if limit == 0 {
			limit = defaultRecentLimit
		}
		// The 1..500 bound is the clip server's and is NOT re-checked here: it
		// answers 400 invalid_limit with a reason, which is relayed verbatim.
		rows, err := cl.list(ctx, limit, in.Query)
		if err != nil {
			return nil, recentOut{}, err
		}
		out := recentOut{Clips: make([]clipOut, 0, len(rows))}
		for _, r := range rows {
			out.Clips = append(out.Clips, toOut(r))
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_clip",
		Description: "Read one clip by its id (from recent_clips). Only the 500 newest clips are reachable.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, clipOut, error) {
		if in.ID < 1 {
			return nil, clipOut{}, fmt.Errorf("id must be a positive integer")
		}
		// 🔴 THE CLIP SERVER HAS NO GET-BY-ID ROUTE (its route ledger is
		// POST/GET /v1/clips and DELETE /v1/clips/{id}). So this reads the
		// newest clipMaxLimit rows and picks one out. A clip older than that is
		// reported as out of reach, NOT as nonexistent: "not found" would be a
		// false statement about a row that may well exist.
		rows, err := cl.list(ctx, clipMaxLimit, "")
		if err != nil {
			return nil, clipOut{}, err
		}
		for _, r := range rows {
			if r.ID == in.ID {
				return nil, toOut(r), nil
			}
		}
		if len(rows) < clipMaxLimit {
			return nil, clipOut{}, fmt.Errorf("no clip with id %d (the store holds %d clips and none has that id)", in.ID, len(rows))
		}
		return nil, clipOut{}, fmt.Errorf("clip %d is not among the %d newest clips; the clip server has no "+
			"fetch-by-id route, so an older clip can only be found with recent_clips and a query", in.ID, clipMaxLimit)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "push_clip",
		Description: "Send text to the user's devices as a new clip on their promptshipper shared clipboard " +
			"(phone app and desktop). Use it to hand the user something to paste: a command, a reply draft, a link.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pushIn) (*mcp.CallToolResult, pushOut, error) {
		if in.Text == "" {
			return nil, pushOut{}, fmt.Errorf("text must not be empty")
		}
		r, err := cl.push(ctx, in.Text, device)
		if err != nil {
			return nil, pushOut{}, err
		}
		return nil, pushOut{ID: r.ID, Deduped: r.Deduped, Note: pushNote}, nil
	})

	return s
}

func ptr[T any](v T) *T { return &v }
