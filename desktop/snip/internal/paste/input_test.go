package paste

import (
	"strings"
	"testing"

	"github.com/ZacxDev/promptshipper/desktop/snip/internal/artifact"
)

func boolPtr(b bool) *bool { return &b }

// 🔴 CLIP-BEFORE-INPUTS ORDERING. {{clip}} is resolved FIRST, then inputs.
// A typed input value that contains the literal text "{{clip}}" must stay
// literal and never resolve to the clipboard contents.
func TestClipBeforeInputsOrdering(t *testing.T) {
	fx := &fakeEffects{
		originExists: true,
		clipOwned:    true,
		clip:         []byte("the-real-clipboard"),
		workspace:    1,
	}
	rows := captureRows(t)

	snippets := []artifact.Snippet{{
		Label: "test",
		Body:  "prefix {{clip}} middle {{field}} suffix",
		Inputs: []artifact.Input{
			{Name: "field", Type: "text", Required: boolPtr(false)},
		},
	}}
	req := Request{
		Label:        "test",
		OriginWindow: 42,
		OriginClass:  "x",
		SetValues:    map[string]string{"field": "literal {{clip}} text"},
	}

	res := Run(fx, snippets, req)
	if res.Outcome != "select" {
		t.Fatalf("Run = %+v, want select", res)
	}
	if len(rows.rows) != 1 {
		t.Fatalf("rows: %d", len(rows.rows))
	}
	expanded := firstSet(t, fx)
	if !strings.Contains(expanded, "the-real-clipboard") {
		t.Errorf("expansion is missing the clip content: %q", expanded)
	}
	if !strings.Contains(expanded, "literal {{clip}} text") {
		t.Errorf("expansion is missing the literal {{clip}} from the input: %q", expanded)
	}
	if strings.Contains(expanded, "literal the-real-clipboard text") {
		t.Errorf("the input's literal {{clip}} was erroneously resolved to the clipboard: %q", expanded)
	}
}
