package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snippets.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const valid = `[
 {"label": "End-of-session ritual", "body": "reflect on work done",
  "search_terms": ["handoff", "session"], "category": "prompt"},
 {"label": "typo fix: dashboard", "body": "dashboard", "search_terms": [], "category": "typo"},
 {"label": "Paste from clipboard", "body": "{{clip}}",
  "search_terms": [], "category": "clipboard"}
]`

func TestLoadValid(t *testing.T) {
	got, err := Load(write(t, valid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	s := got[0]
	if s.Label != "End-of-session ritual" || s.Body != "reflect on work done" ||
		s.Category != "prompt" || len(s.SearchTerms) != 2 {
		t.Fatalf("row 0 mismatched: %+v", s)
	}
	// A missing search_terms array decodes as empty, not an error.
	loose, err := Load(write(t, `[{"label": "a", "body": "x"}]`))
	if err != nil || len(loose) != 1 || len(loose[0].SearchTerms) != 0 {
		t.Fatalf("missing optional fields must decode as empty: %v %+v", err, loose)
	}
}

func TestValidationFailsLoudNamingTheFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantIn  string
	}{
		{"empty label", `[{"label": "", "body": "x"}]`, "empty label"},
		{"duplicate labels", `[{"label": "a", "body": "x"}, {"label": "a", "body": "y"}]`, `duplicate label "a"`},
		{"not an array", `{"label": "a"}`, "not a valid snippets.json array"},
		{"truncated json", `[{"label": "a"`, "not a valid snippets.json array"},
	}
	for _, c := range cases {
		path := write(t, c.content)
		_, err := Load(path)
		if err == nil {
			t.Errorf("%s: Load succeeded, want failure", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantIn) {
			t.Errorf("%s: error %q does not name the cause %q", c.name, err, c.wantIn)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error %q does not name the file %q", c.name, err, path)
		}
	}
}

func TestMissingFileNamesTheFile(t *testing.T) {
	_, err := Load("/nonexistent/snip/snippets.json")
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/snip/snippets.json") {
		t.Fatalf("missing-file error = %v, want one naming the path", err)
	}
}

func TestFindIsExact(t *testing.T) {
	snippets, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := Find(snippets, "Paste from clipboard"); !ok || s.Body != "{{clip}}" {
		t.Fatalf("Find(Paste from clipboard) = %+v %v", s, ok)
	}
	if _, ok := Find(snippets, "Paste from clipboar"); ok {
		t.Fatal("Find must be exact; a substring is not the label")
	}
	if _, ok := Find(snippets, "paste from clipboard"); ok {
		t.Fatal("Find must be exact; the label is case-sensitive")
	}
}

func TestPathResolution(t *testing.T) {
	t.Setenv("SNIP_CONFIG", "/override/path.json")
	if p, _ := Path(); p != "/override/path.json" {
		t.Fatalf("SNIP_CONFIG override ignored: %s", p)
	}
	os.Unsetenv("SNIP_CONFIG")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/cfg")
	if p, _ := Path(); p != "/xdg/cfg/snip/snippets.json" {
		t.Fatalf("XDG_CONFIG_HOME resolution wrong: %s", p)
	}
}

// --- input validation ---

func TestRowsWithoutInputsStillLoad(t *testing.T) {
	got, err := Load(write(t, valid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for _, s := range got {
		if s.HasInputs() {
			t.Fatalf("row %q unexpectedly has inputs", s.Label)
		}
	}
}

var validWithInputs = `[
  {"label": "Deploy", "body": "Deploy {{service}} to {{env}}, then verify {{scopes}}.",
   "search_terms": ["deploy"], "category": "prompt",
   "inputs": [
     {"name": "service", "type": "text", "prompt": "Service", "required": true},
     {"name": "env", "type": "select", "prompt": "Environment", "options": ["staging", "prod"], "default": "staging"},
     {"name": "scopes", "type": "multiselect", "prompt": "Scopes", "options": ["api", "web", "db"], "default": ["api"], "join": ", "}
   ]}
]`

func TestRowWithInputsRoundTrips(t *testing.T) {
	got, err := Load(write(t, validWithInputs))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	s := got[0]
	if !s.HasInputs() {
		t.Fatal("row should have inputs")
	}
	if len(s.Inputs) != 3 {
		t.Fatalf("inputs = %d, want 3", len(s.Inputs))
	}
	if s.Inputs[0].Name != "service" || s.Inputs[0].Type != "text" {
		t.Fatalf("input 0: %+v", s.Inputs[0])
	}
	if s.Inputs[1].Type != "select" || len(s.Inputs[1].Options) != 2 {
		t.Fatalf("input 1: %+v", s.Inputs[1])
	}
	if s.Inputs[2].Type != "multiselect" || s.Inputs[2].Join != ", " {
		t.Fatalf("input 2: %+v", s.Inputs[2])
	}
}

func TestInputValidationNamesTheFileAndRow(t *testing.T) {
	cases := []struct {
		name   string
		json   string
		wantIn string
	}{
		{"unknown type", `[{"label":"x","body":"{{a}}","inputs":[{"name":"a","type":"bogus"}]}]`, `unknown type`},
		{"bad name", `[{"label":"x","body":"{{A}}","inputs":[{"name":"A","type":"text"}]}]`, `does not match`},
		{"reserved", `[{"label":"x","body":"{{clip}}","inputs":[{"name":"clip","type":"text"}]}]`, `reserved`},
		{"duplicate name", `[{"label":"x","body":"{{a}} {{a}}","inputs":[{"name":"a","type":"text"},{"name":"a","type":"text"}]}]`, `duplicate`},
		{"select no options", `[{"label":"x","body":"{{a}}","inputs":[{"name":"a","type":"select"}]}]`, `no options`},
		{"empty option", `[{"label":"x","body":"{{a}}","inputs":[{"name":"a","type":"select","options":[""]}]}]`, `option is empty`},
		{"dup options", `[{"label":"x","body":"{{a}}","inputs":[{"name":"a","type":"select","options":["z","z"]}]}]`, `duplicate option`},
		{"select default not in options", `[{"label":"x","body":"{{a}}","inputs":[{"name":"a","type":"select","options":["a"],"default":"b"}]}]`, `not in options`},
		{"undeclared placeholder", `[{"label":"x","body":"{{typo}}","inputs":[{"name":"a","type":"text"}]}]`, `undeclared`},
		{"never used", `[{"label":"x","body":"hello","inputs":[{"name":"a","type":"text"}]}]`, `never appears`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := write(t, c.json)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load should have failed for %s", c.name)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("error %q does not contain %q", err, c.wantIn)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name the file %q", err, path)
			}
			if !strings.Contains(err.Error(), `"x"`) {
				t.Fatalf("error %q does not name the row label", err)
			}
		})
	}
}
