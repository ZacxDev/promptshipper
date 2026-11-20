package main

import (
	"bytes"
	"io"
	"sync"
)

// redactedMarker replaces the token wherever it appears. It contains no byte a
// JSON string would need escaped, so replacing inside an encoded frame keeps the
// frame valid JSON.
const redactedMarker = "[REDACTED]"

// redactingWriter keeps the clip token's BYTES out of everything this process
// emits. Both output streams go through one: stdout (every MCP frame — tool
// results, errors, notifications) and stderr (every log line, ours and the
// SDK's).
//
// 🔴 WHAT IT DOES NOT PROTECT: INFORMATION ABOUT THE TOKEN. It is an output
// filter; it cannot stop an agent from LEARNING the token through behaviour
// that depends on it. The demonstrated case: the agent's search query used to
// go to the clip server, whose substring match is a yes/no answer per query,
// and extending a query one character at a time recovered a stored token
// exactly while every result showed [REDACTED]. That oracle is closed in
// tools.go (recentClips: the query never leaves this process and is matched
// only against already-redacted bodies; oracle_test.go), not here. Any new
// tool or parameter that lets the CLIP SERVER compute something over raw clip
// bodies reopens it, whatever this writer does.
//
// WHY THE BYTE FILTER IS HERE AND NOT IN EACH HANDLER. The token can reach
// output by paths no handler author writes on purpose: a clip containing the
// token (the user copied it on their phone), a misconfigured URL pointing at
// something that echoes request headers back in its error body, an error
// string from the HTTP stack. Filtering the encoded bytes on their way out
// covers all of them, including ones added later. Clip bodies are ALSO
// redacted at the source (tools.go) so that the local search and the body cap
// only ever see redacted text.
//
// The match is ASCII-CASE-INSENSITIVE: a hex token in upper case is the same
// secret to anyone who reads it.
//
// 🔴 WHY A BYTE REPLACE IS COMPLETE. config.go refuses any token outside
// [A-Za-z0-9._~-], and a JSON encoder never escapes those characters, so a
// token inside any JSON string appears in the frame as its exact bytes. If the
// alphabet is ever widened, this stops being sufficient — see tokenRe.
//
// It buffers to the newline. MCP stdio frames and log lines are both
// newline-delimited, so a token can never be split across two flushes; a
// writer that redacted each Write call independently would leak a token that
// straddled two of them.
type redactingWriter struct {
	mu     sync.Mutex
	out    io.Writer
	secret []byte
	buf    []byte
}

func newRedactingWriter(out io.Writer, secret string) *redactingWriter {
	return &redactingWriter{out: out, secret: []byte(secret)}
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	i := bytes.LastIndexByte(w.buf, '\n')
	if i < 0 {
		return len(p), nil
	}
	line := w.buf[:i+1]
	if _, err := w.out.Write(redactBytes(line, w.secret)); err != nil {
		return 0, err
	}
	w.buf = append(w.buf[:0], w.buf[i+1:]...)
	return len(p), nil
}

// Close flushes a trailing partial line (redacted) and closes out if it can.
func (w *redactingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		if _, err := w.out.Write(redactBytes(w.buf, w.secret)); err != nil {
			return err
		}
		w.buf = w.buf[:0]
	}
	if c, ok := w.out.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// redactBytes replaces every ASCII-case-insensitive occurrence of secret.
// The token alphabet is ASCII (config.go tokenRe), so folding ASCII only keeps
// byte offsets in the folded copy aligned with the original.
func redactBytes(b, secret []byte) []byte {
	if len(secret) == 0 {
		return b
	}
	lb, ls := asciiLower(b), asciiLower(secret)
	if !bytes.Contains(lb, ls) {
		return b
	}
	out := make([]byte, 0, len(b))
	i := 0
	for {
		j := bytes.Index(lb[i:], ls)
		if j < 0 {
			break
		}
		out = append(out, b[i:i+j]...)
		out = append(out, redactedMarker...)
		i += j + len(ls)
	}
	return append(out, b[i:]...)
}

func redactString(s, secret string) string {
	return string(redactBytes([]byte(s), []byte(secret)))
}

func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}
