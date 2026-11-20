// clipstore — the cross-device shared clipboard store.
//
// Highlight text on any device, it is on every other device. This is the SERVER
// half: a single-writer SQLite store with an FTS5 index, three bearer-gated HTTP
// operations plus an unauthenticated /healthz, and nothing else. The API is
// documented in README.md beside this file.
//
// WHAT IT DELIBERATELY DOES NOT HAVE, so nobody adds it back by accident:
//
//   - NO EXPIRY SWEEP. the design decision is "no expiry, manual delete only".
//     If expiry is ever wanted it belongs in-process on write+startup, NEVER as
//     a cron container: that would be a second WRITE-CAPABLE process against
//     a single-writer WAL SQLite file, which is the one thing running exactly
//     one clipstore per database exists to prevent.
//   - NO LABELS OR CATEGORIES. By design this is a clipboard, chronological
//     and unlabelled. The labelled store is the separate nix snip catalog, and
//     the two must not be conflated.
//   - NO TLS, NO PUBLIC EXPOSURE BUILT IN. It is meant to be reached over a
//     private network (a Tailscale tailnet, a LAN, a VPN). Never publish it
//     to the internet without a TLS-terminating proxy in front of it.
//   - NO `-healthcheck` SELF-PROBE MODE. It existed only because /healthz was
//     bearer-gated and an httpGet probe cannot carry a secret header without
//     committing it in plaintext. /healthz is unauthenticated now, the probes
//     are plain HTTP GETs, and the whole self-probe mechanism is gone. Do not
//     re-add it; re-gating /healthz is what would require it back.
//
// 🔴 EVERY FAILURE IS LOUD. An unreadable store answers 5xx with a named
// reason; it never answers `[]`. An empty result is indistinguishable from a
// working feature with nothing in it, and the clients are built on the
// assumption that this server will not make that mistake.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// writeTimeout is the server's per-response write deadline; see main.
const writeTimeout = (MaxWaitSeconds + 30) * time.Second

// tokenEnv is the only way the bearer token is supplied. There is no flag for
// it: a flag value is visible in the pod's argv and in `kubectl describe`.
const tokenEnv = "CLIPSTORE_TOKEN"

// buildVersion is stamped by the Dockerfile's `-ldflags -X main.buildVersion`.
// It is logged at startup so a pod's log answers "which artifact is this" —
// which an image tag alone does not, since a tag is what the manifest ASKED for
// and the log is what actually started.
var buildVersion = "dev"

// tokenRefusal returns the FATAL diagnostic for a bearer token that does not
// clear MinTokenBytes, or "" when it does. main prints it to stderr and exits 2.
//
// 🔴 IT IS A FUNCTION RATHER THAN FOUR LINES INSIDE main SO IT IS REACHABLE FROM
// A TEST. The check was previously inline, main.go had no test file at all, and
// mutating MinTokenBytes from 24 to 0 SURVIVED the whole suite at pass=149
// fail=0 — a 🔴 guard that was never once executed by a test.
// TestAShortTokenIsRefusedWithADiagnosticNamingTheSecret and
// TestTheTokenFloorIsAtLeastEightBytes now kill that mutant, and
// TestMainExitsTwoOnAShortToken re-execs this binary to pin the exit code.
//
// 🔴 WHAT THE FLOOR ACTUALLY BUYS, CORRECTED. This comment used to say the
// failure it closes is that "an empty token with a `==` comparison would accept
// `Authorization: Bearer ` from any mesh peer". THAT FAILURE IS ALREADY CLOSED
// TWICE OVER, in handlers.go, and the floor is not what closes it:
//
//   - `bearer()` rejects an empty credential outright — `Bearer `, `Bearer` and
//     `Bearer   ` all return ok=false before any comparison happens. (Which
//     matters more than it looks: subtle.ConstantTimeCompare of two EMPTY
//     slices returns 1, so bearer() is the only thing standing there.)
//   - subtle.ConstantTimeCompare returns 0 immediately on a length mismatch, so
//     a non-empty credential cannot match an empty configured token either.
//
// TestAnEmptyConfiguredTokenAuthenticatesNothing pins both, so the corrected
// claim is tested rather than asserted. An empty token therefore authenticates
// NOTHING rather than everything: the pod 401s every legitimate client.
//
// So what the floor buys is DIAGNOSIS, not authorization: it turns a missing or
// truncated Secret into a CrashLoopBackOff naming the Secret, instead of a pod
// that passes its probes (/healthz is unauthenticated) and 401s every client
// while looking perfectly healthy. Second, it refuses a short-but-non-empty
// token, which IS a real authorization fact: the listener is reachable from
// every host on the mesh, so a guessable credential is a live exposure. That is
// the posture a minimum token length of 8 states, and this is stricter.
func tokenRefusal(token string) string {
	if len(token) >= MinTokenBytes {
		return ""
	}
	return fmt.Sprintf(
		"FATAL: $%s is %d bytes; at least %d are required.\n"+
			"Set it in deploy/compose/.env (./setup generates one), e.g.\n"+
			"  CLIPSTORE_TOKEN=$(openssl rand -hex 32)\n"+
			"Refusing to start rather than serve a clipboard that 401s every client.\n",
		tokenEnv, len(token), MinTokenBytes)
}

// tokenScanner is the slice of the store the startup scan needs.
type tokenScanner interface {
	CountContaining(ctx context.Context, needle string) (int64, error)
}

// reportTokenClips is the one-time startup scan: it reports HOW MANY stored
// clips contain this server's own token — never which, never their content,
// never the token — and that the token must be ROTATED. Deleting those clips
// is not the fix: a token that sat in a readable clip may already have been
// read, and only rotation retires it. POST refuses such clips (422), so any
// found predate that guard. A scan failure is logged and does not stop the
// server: it is a hygiene report, not a precondition.
func reportTokenClips(ctx context.Context, s tokenScanner, token string, out io.Writer) {
	n, err := s.CountContaining(ctx, token)
	switch {
	case err != nil:
		fmt.Fprintf(out, "clipstore: WARNING: could not scan stored clips for this server's token: %v\n", err)
	case n > 0:
		fmt.Fprintf(out, "clipstore: WARNING: %d stored clip(s) contain this server's own access token. "+
			"Anything that can read clips may already have read it: rotate the token with "+
			"./setup --rotate (then re-pair your devices).\n", n)
	}
}

func main() {
	addr := flag.String("addr", ":8139",
		"listen address. 8139 is the port the container image exposes.")
	dbPath := flag.String("db", "/data/clipstore.db",
		"SQLite database path. Must be on a persistent volume; /data is the only "+
			"writable mount besides /tmp in the container image.")
	flag.Parse()

	token := os.Getenv(tokenEnv)
	if msg := tokenRefusal(token); msg != "" {
		fmt.Fprint(os.Stderr, msg)
		os.Exit(2)
	}

	store, err := OpenStore(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(2)
	}
	defer store.Close()

	reportTokenClips(context.Background(), store, token, os.Stderr)

	api := NewAPI(store, token, time.Now)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A clip body can be 256 KiB over a VPN link from a phone on mobile
		// data; 60s is generous for that and still bounded.
		ReadTimeout: 60 * time.Second,
		// Must outlast the longest long-poll (MaxWaitSeconds) plus the write of
		// its answer, or the server would cut every full-length wait.
		// TestTheWriteTimeoutOutlastsTheLongestWait pins it.
		WriteTimeout: writeTimeout,
		IdleTimeout:  120 * time.Second,
	}

	// SIGTERM handling exists so a rollout closes the WAL cleanly. It is not
	// load-bearing — journal_mode=WAL makes an abrupt stop crash-safe, which is
	// why an abrupt container stop is acceptable — but a clean
	// checkpoint leaves a single-file database behind, which makes the next
	// backup's work smaller.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Release long-poll waiters first, or Shutdown sits out a 60 s wait.
		api.Shutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("clipstore: shutdown: %v", err)
		}
	}()

	log.Printf("clipstore %s: listening on %s, db=%s (limit default %d, max %d)",
		buildVersion, *addr, *dbPath, DefaultLimit, MaxLimit)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("FATAL: listen on %s: %v", *addr, err)
		os.Exit(1)
	}
	log.Printf("clipstore: stopped")
}
