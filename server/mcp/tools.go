package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// searchWindow is how many of the newest clips recent_clips fetches, redacts
// and searches: the clip server's hard cap on ?limit=. SEARCH COVERS ONLY
// THESE; an older clip is not findable through this server. That is the price
// of not sending the query to the clip server (see recentClips).
const searchWindow = 500

// maxClipBodyBytes caps each returned body so that 20 large clips (the clip
// server allows 256 KiB each) cannot flood the agent's context. A capped body
// carries truncated: true and its full size.
const maxClipBodyBytes = 16 << 10

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
	Truncated bool   `json:"truncated,omitempty" jsonschema:"the body was cut to 16 KiB; body_bytes is its full size"`
	BodyBytes int    `json:"body_bytes,omitempty"`
}

// toOut builds the agent-facing row from an ALREADY-REDACTED clip, so the body
// cap can never cut a token in half and leave a prefix nothing recognises.
func toOut(c clip) clipOut {
	o := clipOut{
		ID: c.ID, Body: c.Body, Device: c.Device, BumpCount: c.BumpCount,
		CreatedAt: time.UnixMilli(c.CreatedAt).UTC().Format(time.RFC3339),
	}
	if len(o.Body) > maxClipBodyBytes {
		o.Truncated, o.BodyBytes = true, len(o.Body)
		o.Body = strings.ToValidUTF8(o.Body[:maxClipBodyBytes], "")
	}
	return o
}

// matches is the local search: every whitespace-separated query word must
// appear in the body as a case-insensitive substring (the clip server's own
// contract), applied here to the REDACTED body.
func matches(body string, words []string) bool {
	lb := strings.ToLower(body)
	for _, w := range words {
		if !strings.Contains(lb, w) {
			return false
		}
	}
	return true
}

type recentIn struct {
	Limit int    `json:"limit,omitempty" jsonschema:"how many clips to return, newest first: 1..500, default 20"`
	Query string `json:"query,omitempty" jsonschema:"optional search over the newest 500 clips only: every whitespace-separated word must appear in the clip as a case-insensitive substring"`
}

type recentOut struct {
	Clips []clipOut `json:"clips"`
	// Searched is how many of the newest clips were fetched and filtered: the
	// search's whole scope. Older clips are not searched.
	Searched int `json:"searched" jsonschema:"how many of the newest clips the query was matched against; older clips were not searched"`
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
			"\"send dictations to my clip feed\" in the app. Optionally filter with a search query, which " +
			"covers only the newest 500 clips (the result's searched field).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recentIn) (*mcp.CallToolResult, recentOut, error) {
		return recentClips(ctx, cl, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "push_clip",
		Description: "Send text to the user's devices as a new clip on their promptshipper shared clipboard " +
			"(phone app and desktop); the phone copies it to its clipboard when the app is running. " +
			"Use it to hand the user something to paste: a command, a reply draft, a link.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
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

// recentClips serves recent_clips.
//
// 🔴 THE QUERY NEVER LEAVES THIS PROCESS. Sending it to the clip server made
// every query a yes/no question about RAW bodies: a token oracle that output
// redaction cannot see (oracle_test.go reproduces the demonstrated attack).
// So: fetch the newest rows with no query, redact them, THEN filter locally
// over the redacted text, THEN apply limit. The order is the fix:
//   - filtering before redacting reopens the oracle;
//   - letting the server apply limit to a query-filtered set, or refusing
//     token-shaped queries, leaks through what is or is not returned.
//
// The fetched window does not depend on the query. It shrinks only when the
// response is too large, which depends on stored sizes, never on the query.
func recentClips(ctx context.Context, cl *clipClient, in recentIn) (*mcp.CallToolResult, recentOut, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultRecentLimit
	}
	if limit < 1 || limit > searchWindow {
		return nil, recentOut{}, fmt.Errorf("limit must be an integer in 1..%d", searchWindow)
	}
	var rows []clip
	window := searchWindow
	for {
		var err error
		rows, err = cl.list(ctx, window)
		if errors.Is(err, errResponseTooLarge) && window > 1 {
			window /= 2
			continue
		}
		if err != nil {
			return nil, recentOut{}, err
		}
		break
	}
	words := strings.Fields(strings.ToLower(in.Query))
	out := recentOut{Clips: []clipOut{}, Searched: len(rows)}
	for _, r := range rows {
		r.Body = redactString(r.Body, cl.token)
		r.Device = redactString(r.Device, cl.token)
		if !matches(r.Body, words) {
			continue
		}
		out.Clips = append(out.Clips, toOut(r))
		if len(out.Clips) == limit {
			break
		}
	}
	return nil, out, nil
}

func ptr[T any](v T) *T { return &v }
