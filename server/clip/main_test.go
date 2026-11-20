// main tests.
//
// 🔴 WHY THIS FILE EXISTS AT ALL. main.go had NO test file, and the consequence
// was measured rather than guessed: mutating MinTokenBytes from 24 to 0 SURVIVED
// the entire suite at pass=149 fail=0. The guard main.go's own comment marks 🔴
// ("A MISSING OR SHORT TOKEN IS A REFUSAL TO START") had never been executed by
// a test. Everything here is an INVARIANT GUARD — main.go is new and nothing has
// shipped, so none of it is regression coverage for a bug that reached anyone.
//
// 🔴 AND THE CLAIM IT PINS IS THE CORRECTED ONE. The old comment said the floor
// closes "an empty token with a `==` comparison would accept
// `Authorization: Bearer ` from any mesh peer". That failure is closed twice over
// in handlers.go, by bearer() and by ConstantTimeCompare's length check, and
// TestAnEmptyConfiguredTokenAuthenticatesNothing (handlers_test.go) pins both.
// What the floor buys is a LOUD DIAGNOSIS instead of a pod that passes its
// unauthenticated /healthz probes while 401-ing every client, plus the refusal
// of a short-but-non-empty credential on a listener every mesh host can reach.
// tokenRefusal's comment states it; these tests are what make it checkable.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// reexecEnv makes this test binary run main() instead of the tests, which is how
// the exit CODE is observed. There is no other way: os.Exit(2) cannot be
// intercepted in-process.
const reexecEnv = "CLIPSTORE_TEST_REEXEC_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(reexecEnv) == "1" {
		main()
		// Only reachable if main() returned without exiting, which for the
		// refusal fixtures below means the refusal did not happen. Exit 0 so the
		// parent sees "it did not exit 2" rather than a confusing signal.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// minTokenLenPrecedent is the posture handlers.go's MinTokenBytes comment
// cites: a minimum token length of 8. It is spelled here as an
// independent number rather than derived from MinTokenBytes, because a floor
// asserted against itself is not a floor.
const minTokenLenPrecedent = 8

// 🔴 THE MUTANT-KILLER FOR MinTokenBytes ITSELF. At 0 the comparison
// `len(token) < MinTokenBytes` is never true for any input, so the guard stops
// existing without any test having to change — which is exactly what was
// measured. This asserts the VALUE, which is the state, rather than asserting
// that some token is refused (a claim a lowered-but-nonzero floor could still
// satisfy).
func TestTheTokenFloorIsAtLeastEightBytes(t *testing.T) {
	if MinTokenBytes < minTokenLenPrecedent {
		t.Fatalf("MinTokenBytes = %d, below the minimum length of %d that "+
			"handlers.go's comment names as the posture this mirrors. At 0 it is not a floor "+
			"at all: `len(token) < 0` is false for every input, so a missing or truncated "+
			"Secret would start the pod, pass its unauthenticated /healthz probes, and 401 "+
			"every client — the silent configuration failure this guard exists to make loud",
			MinTokenBytes, minTokenLenPrecedent)
	}
}

// Invariant guard on the refusal itself, at the boundary and well off it.
//
// 🔴 EACH FIXTURE DECLARES ITS OWN LENGTH AND THE TEST CHECKS IT. A boundary
// test whose fixture is the wrong length tests the wrong boundary and still
// passes, so the literal is validated before it is used. The lengths are
// pairwise distinct, and two of them (0 and 43) are nowhere near MinTokenBytes,
// so a mutant that moves the floor a little is caught by the boundary pair while
// a mutant that removes it is caught by the far cases.
func TestAShortTokenIsRefusedWithADiagnosticNamingTheSecret(t *testing.T) {
	for _, tc := range []struct {
		name        string
		token       string
		bytes       int
		wantRefusal bool
	}{
		{"absent Secret", "", 0, true},
		{"a stray single character", "x", 1, true},
		{"sixteen bytes", "sixteen-byte-tok", 16, true},
		{"one byte under the floor", "twenty-three-bytes-long", 23, true},
		{"exactly the floor", "twenty-four-bytes-long!!", 24, false},
		{"the provisioned shape: 32 bytes of base64url", "9cNfQ2pLm4vR8sT1yU6wZ3aB5cD0eF7gH-jIkMn", 39, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.token) != tc.bytes {
				t.Fatalf("fixture %q is %d bytes, not the %d this case means to exercise — the "+
					"boundary being tested is not the one written down",
					tc.token, len(tc.token), tc.bytes)
			}
			msg := tokenRefusal(tc.token)
			if tc.wantRefusal && msg == "" {
				t.Fatalf("tokenRefusal(a %d-byte token) = \"\", so main() would START. "+
					"MinTokenBytes is %d, so this must be a refusal", tc.bytes, MinTokenBytes)
			}
			if !tc.wantRefusal && msg != "" {
				t.Fatalf("tokenRefusal(a %d-byte token) refused with %q, but MinTokenBytes is "+
					"%d — a valid token must not be refused, or the Secret rotation that "+
					"provisions one bricks the pod", tc.bytes, msg, MinTokenBytes)
			}
			if !tc.wantRefusal {
				return
			}
			// The diagnostic is the whole point of refusing rather than running:
			// it has to be readable in `kubectl logs` by someone who does not
			// know this code, and it must name WHERE the Secret comes from.
			for _, frag := range []string{
				"FATAL",
				tokenEnv,
				"deploy/compose/.env",
				"./setup",
			} {
				if !strings.Contains(msg, frag) {
					t.Errorf("the refusal diagnostic does not contain %q. It is the only thing "+
						"a CrashLoopBackOff gives the operator, so it has to name the Secret "+
						"and the variable.\nGot:\n%s", frag, msg)
				}
			}
			// It must also say what was wrong, not just that something was.
			if !strings.Contains(msg, "0 bytes") && !strings.Contains(msg, "bytes;") {
				t.Errorf("the diagnostic does not report the observed length: %s", msg)
			}
		})
	}
}

// 🔴 THE EXIT CODE, OBSERVED RATHER THAN INFERRED. tokenRefusal returning a
// non-empty string is not the same claim as "the process refuses to start":
// main() has to actually print it and exit non-zero, and `os.Exit(2)` is a line
// no in-process test can reach. This re-execs the test binary with the env guard
// TestMain reads, so the thing under test is the real main().
//
// 2 specifically, because main() also exits 2 for an unopenable store and the
// Deployment treats both as "configuration is wrong, CrashLoopBackOff".
func TestMainExitsTwoOnAShortToken(t *testing.T) {
	for _, tok := range []string{"", "far-too-short"} {
		t.Run("len="+itoa(len(tok)), func(t *testing.T) {
			// 🔴 SAFETY INTERLOCK, and it is load-bearing for the test SUITE, not
			// just this case. main() with an ACCEPTABLE token proceeds to
			// OpenStore and then blocks in ListenAndServe forever, so a fixture
			// that stopped being a refusal case would hang the subprocess instead
			// of failing. Assert the premise before spawning.
			if tokenRefusal(tok) == "" {
				t.Fatalf("fixture %q clears MinTokenBytes=%d, so the subprocess would reach "+
					"ListenAndServe and HANG rather than exit. Use a token this build refuses",
					tok, MinTokenBytes)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// No arguments: main() must refuse before it ever looks at -db or
			// -addr, so passing a path or a port would weaken the claim.
			cmd := exec.CommandContext(ctx, os.Args[0])
			// 🔴 THE PARENT'S OWN $CLIPSTORE_TOKEN IS STRIPPED, NOT SHADOWED.
			// os/exec's dedupEnv keeps the LAST duplicate, so appending would
			// work today — but that is an implementation detail to depend on for
			// a test whose whole subject is the value of this variable, and an
			// operator running the suite on a host that exports a real token
			// would otherwise silently test the wrong input.
			env := make([]string, 0, len(os.Environ())+2)
			for _, kv := range os.Environ() {
				if strings.HasPrefix(kv, tokenEnv+"=") || strings.HasPrefix(kv, reexecEnv+"=") {
					continue
				}
				env = append(env, kv)
			}
			cmd.Env = append(env, reexecEnv+"=1", tokenEnv+"="+tok)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("re-exec of main() with a %d-byte token returned err=%v, so it did "+
					"NOT exit non-zero. The pod would start and serve a clipboard that 401s "+
					"every client.\nstdout: %s\nstderr: %s",
					len(tok), err, stdout.String(), stderr.String())
			}
			if got := exitErr.ExitCode(); got != 2 {
				t.Fatalf("main() exited %d with a %d-byte token, want 2.\nstderr: %s",
					got, len(tok), stderr.String())
			}
			// The diagnostic has to reach STDERR specifically — stdout is where
			// the log lines go and a FATAL on stdout is easy to miss.
			if !strings.Contains(stderr.String(), tokenEnv) {
				t.Errorf("the refusal did not name $%s on stderr.\nstderr: %s\nstdout: %s",
					tokenEnv, stderr.String(), stdout.String())
			}
			if !strings.Contains(stderr.String(), "deploy/compose/.env") {
				t.Errorf("the refusal did not name where the token is set on stderr: %s", stderr.String())
			}
		})
	}
}

// itoa avoids pulling strconv in for one subtest name.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
