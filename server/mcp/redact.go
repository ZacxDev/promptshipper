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

// redactingWriter is THE choke point that keeps the clip token out of
// everything this process emits. Both of the process's output streams go
// through one: stdout (every MCP frame — tool results, errors, notifications)
// and stderr (every log line, ours and the SDK's).
//
// 🔴 WHY HERE AND NOT IN EACH TOOL HANDLER. The token can reach output by paths
// no handler author writes on purpose: a clip whose body is the token (the user
// copied it on their phone), a misconfigured URL pointing at something that
// echoes request headers back in its error body, an error string from the HTTP
// stack. Redacting per field would need every such path to be remembered.
// Redacting the encoded bytes on their way out covers all of them, including
// ones added later, by construction.
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

func redactBytes(b, secret []byte) []byte {
	if len(secret) == 0 {
		return b
	}
	return bytes.ReplaceAll(b, secret, []byte(redactedMarker))
}
