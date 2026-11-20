package main

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tool set is deliberately two tools, one per direction. There is no tool
// for DELETE /v1/clips/{id} (an agent deleting the user's clipboard history is
// a destructive action with no use in the read/push loop this server exists
// for), no get-by-id tool (the clip server has no such route yet; re-add one on
// GET /v1/clips/{id} when it exists), and none for the STT server, which keeps
// no history.
//
// 🔴 WHAT recent_clips CAN SEE: text the user highlighted, shared or copied
// into the clip feed — and their dictations ONLY when they have enabled "send
// dictations to my clip feed" in the app. Without that opt-in a dictation
// never reaches the clip server, so the tool descriptions must not promise it.

// pushDevice is the `device` attribution on every clip this server pushes. It
// is a CONSTANT, not configuration: the app auto-copies clips whose device is
// exactly "mcp" to the phone's clipboard, so a configurable value would be a
// way to silently turn that off.
const pushDevice = "mcp"

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

type pushIn struct {
	Text string `json:"text" jsonschema:"the text to send to the user's devices (max 256 KiB)"`
}

type pushOut struct {
	ID      int64  `json:"id"`
	Deduped bool   `json:"deduped" jsonschema:"true when identical text was already stored and was moved to the top instead of duplicated"`
	Note    string `json:"note"`
}

// pushNote is returned with every push so the agent states the delivery
// condition rather than promising more. See README "Push to the phone".
const pushNote = "Stored as the newest clip (device \"mcp\"). The phone copies it to its clipboard automatically " +
	"only while the promptshipper app's bubble is running and the app version supports the clip feed; " +
	"otherwise it waits at the top of the app's clip list (and snip) for the user to copy."

func newServer(cl *clipClient, opts *mcp.ServerOptions) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "promptshipper", Version: buildVersion}, opts)

	mcp.AddTool(s, &mcp.Tool{
		Name: "recent_clips",
		Description: "Read what the user recently highlighted, shared or copied on their phone and desktop " +
			"(the promptshipper shared clipboard), newest first. Includes their dictations only if they enabled " +
			"\"send dictations to my clip feed\" in the app. Optionally filter with a search query.",
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
		Name: "push_clip",
		Description: "Send text to the user's devices as a new clip on their promptshipper shared clipboard " +
			"(phone app and desktop); the phone copies it to its clipboard when the app is running. " +
			"Use it to hand the user something to paste: a command, a reply draft, a link.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pushIn) (*mcp.CallToolResult, pushOut, error) {
		if in.Text == "" {
			return nil, pushOut{}, fmt.Errorf("text must not be empty")
		}
		r, err := cl.push(ctx, in.Text, pushDevice)
		if err != nil {
			return nil, pushOut{}, err
		}
		return nil, pushOut{ID: r.ID, Deduped: r.Deduped, Note: pushNote}, nil
	})

	return s
}

func ptr[T any](v T) *T { return &v }
