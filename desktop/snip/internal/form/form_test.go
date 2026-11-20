package form

import (
	"strings"
	"testing"

	"github.com/ZacxDev/promptshipper/desktop/snip/internal/artifact"
)

func mustBool(b bool) *bool { return &b }

var deployInputs = []artifact.Input{
	{Name: "service", Type: "text", Prompt: "Service", Required: mustBool(true)},
	{Name: "env", Type: "select", Prompt: "Environment", Options: []string{"staging", "prod"}, Default: "staging"},
	{Name: "scopes", Type: "multiselect", Prompt: "Scopes", Options: []string{"api", "web", "db"}, Default: "api", Join: ", "},
}

const deployBody = "Deploy {{service}} to {{env}}, then verify {{scopes}}."

func TestValidateHappyPath(t *testing.T) {
	if err := Validate(deployBody, deployInputs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateUndeclaredPlaceholder(t *testing.T) {
	body := "Deploy {{service}} to {{env}} and {{typo}}."
	err := Validate(body, deployInputs)
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("want undeclared error, got %v", err)
	}
}

func TestValidateDeclaredNeverUsed(t *testing.T) {
	body := "Deploy {{service}} only."
	err := Validate(body, deployInputs) // env and scopes never used
	if err == nil || !strings.Contains(err.Error(), "never appears") {
		t.Fatalf("want 'never appears' error, got %v", err)
	}
}

func TestValidateClipIsNotAnInput(t *testing.T) {
	// {{clip}} should pass through validation without being an input.
	body := "Deploy {{service}} using clipboard {{clip}}."
	inputs := []artifact.Input{
		{Name: "service", Type: "text", Prompt: "Service", Required: mustBool(true)},
	}
	if err := Validate(body, inputs); err != nil {
		t.Fatalf("{{clip}} should not trigger undeclared error: %v", err)
	}
}

func TestApplyHappyPath(t *testing.T) {
	values := map[string]string{
		"service": "myapp",
		"env":     "prod",
		"scopes":  "db, web",
	}
	result, err := Apply(deployBody, deployInputs, values)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "Deploy myapp to prod, then verify db, web."
	if result != want {
		t.Fatalf("Apply = %q, want %q", result, want)
	}
}

func TestApplyEveryOccurrence(t *testing.T) {
	body := "{{thing}} and {{thing}}"
	inputs := []artifact.Input{{Name: "thing", Type: "text", Required: mustBool(true)}}
	result, err := Apply(body, inputs, map[string]string{"thing": "X"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result != "X and X" {
		t.Fatalf("every occurrence not replaced: %q", result)
	}
}

func TestApplyRequiredEmpty(t *testing.T) {
	values := map[string]string{"service": ""}
	_, err := Apply(deployBody, deployInputs, values)
	if err == nil || !strings.Contains(err.Error(), "required") || !strings.Contains(err.Error(), "service") {
		t.Fatalf("want required error naming service, got %v", err)
	}
}

func TestApplyUnknownKey(t *testing.T) {
	values := map[string]string{"service": "x", "nope": "x"}
	_, err := Apply(deployBody, deployInputs, values)
	if err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want unknown-key error, got %v", err)
	}
}

func TestApplyMultiselectJoin(t *testing.T) {
	inputs := []artifact.Input{
		{Name: "scopes", Type: "multiselect", Options: []string{"api", "web", "db"}, Join: " | "},
	}
	body := "{{scopes}}"
	result, err := Apply(body, inputs, map[string]string{"scopes": "api | web"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result != "api | web" {
		t.Fatalf("got %q", result)
	}
}

func TestApplyMultiselectEmptyIsLegal(t *testing.T) {
	inputs := []artifact.Input{
		{Name: "tags", Type: "multiselect", Options: []string{"a", "b"}},
	}
	body := "tags: {{tags}}"
	result, err := Apply(body, inputs, map[string]string{"tags": ""})
	if err != nil {
		t.Fatalf("empty multiselect: %v", err)
	}
	if result != "tags: " {
		t.Fatalf("got %q", result)
	}
}

// A typed value containing the literal text {{clip}} must stay literal — clip is
// resolved FIRST (before Apply), so the text the operator typed into the form is
// what gets substituted. The test feeds a value that looks like a clip variable
// and asserts it is NOT re-expanded by Apply.
func TestClipInsideValueStaysLiteral(t *testing.T) {
	body := "paste: {{text}}"
	inputs := []artifact.Input{
		{Name: "text", Type: "text", Required: mustBool(true)},
	}
	values := map[string]string{"text": "literal {{clip}} stays"}
	result, err := Apply(body, inputs, values)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result != "paste: literal {{clip}} stays" {
		t.Fatalf("result = %q, want the literal {{clip}} text preserved", result)
	}
}

func TestPromptFor(t *testing.T) {
	if got := PromptFor(artifact.Input{Name: "svc"}); got != "svc" {
		t.Fatalf("fallback to name: got %q", got)
	}
	if got := PromptFor(artifact.Input{Name: "svc", Prompt: "Service"}); got != "Service" {
		t.Fatalf("use prompt: got %q", got)
	}
}

func TestJoinFor(t *testing.T) {
	if got := JoinFor(artifact.Input{}); got != ", " {
		t.Fatalf("default join: got %q", got)
	}
	if got := JoinFor(artifact.Input{Join: " | "}); got != " | " {
		t.Fatalf("explicit join: got %q", got)
	}
}

func TestEffectiveDefault(t *testing.T) {
	// text with default
	if got := EffectiveDefault(artifact.Input{Type: "text", Default: "hi"}); got != "hi" {
		t.Fatalf("text default: got %q", got)
	}
	// text without default
	if got := EffectiveDefault(artifact.Input{Type: "text"}); got != "" {
		t.Fatalf("text no default: got %q", got)
	}
	// select with default
	if got := EffectiveDefault(artifact.Input{Type: "select", Options: []string{"a", "b"}, Default: "b"}); got != "b" {
		t.Fatalf("select default: got %q", got)
	}
	// select without default -> first option
	if got := EffectiveDefault(artifact.Input{Type: "select", Options: []string{"a", "b"}}); got != "a" {
		t.Fatalf("select fallback: got %q", got)
	}
	// multiselect
	if got := EffectiveDefault(artifact.Input{Type: "multiselect", Options: []string{"a", "b"}, Default: []any{"a", "b"}}); got != "a, b" {
		t.Fatalf("multiselect default: got %q", got)
	}
	// multiselect no default
	if got := EffectiveDefault(artifact.Input{Type: "multiselect", Options: []string{"x"}}); got != "" {
		t.Fatalf("multiselect no default: got %q", got)
	}
}

func TestValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		inputs []artifact.Input
		wantIn string
	}{
		{"undeclared placeholder", "{{svc}}", []artifact.Input{{Name: "service", Type: "text", Required: mustBool(true)}}, "undeclared"},
		{"never used", "hello", []artifact.Input{{Name: "svc", Type: "text", Required: mustBool(true)}}, "never appears"},
		{"ok both ways", "{{svc}} world", []artifact.Input{{Name: "svc", Type: "text", Required: mustBool(true)}}, ""},
		{"clip not an input", "{{clip}}", nil, ""},
		{"clip with input", "{{clip}} {{svc}}", []artifact.Input{{Name: "svc", Type: "text", Required: mustBool(true)}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.body, c.inputs)
			if c.wantIn == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			} else if c.wantIn != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantIn) {
					t.Fatalf("want error containing %q, got %v", c.wantIn, err)
				}
			}
		})
	}
}
