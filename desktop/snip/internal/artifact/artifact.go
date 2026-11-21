// Package artifact loads the snip catalog: ~/.config/snip/snippets.json, or
// wherever $SNIP_CONFIG points. The catalog is a plain JSON array, typically
// generated from a declarative source (nix/home-manager) at deploy time. It is
// read AT PROCESS START, EVERY invocation — no daemon, no watching: a config
// change is picked up by the next fire.
package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// InputNamePattern is the allowed shape for an input name: lowercase letters,
// digits and underscore, must start with a letter.
var InputNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Input is one declared, typed input on a snippet row. The picker shows a form;
// the CLI takes --set name=value; the paste helper substitutes placeholders.
type Input struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // text | select | multiselect
	Prompt   string   `json:"prompt,omitempty"`
	Default  any      `json:"default,omitempty"`  // string for text, string for select, []string for multiselect
	Required *bool    `json:"required,omitempty"` // nil means default (true for text, N/A for select/multiselect)
	Options  []string `json:"options,omitempty"`
	Join     string   `json:"join,omitempty"` // multiselect join delimiter, default ", "
}

// IsRequired reports the effective required-ness:
//   - text: defaults to true when required is absent.
//   - select: always true (required by definition).
//   - multiselect: always false (zero selected is legal — substitutes empty).
func (in *Input) IsRequired() bool {
	switch in.Type {
	case "select":
		return true
	case "multiselect":
		return false
	default:
		if in.Required != nil {
			return *in.Required
		}
		return true
	}
}

// Snippet is one row of the catalog — the exact JSON shape:
// {label, body, search_terms, category, inputs?}. The LABEL is the row's
// identity (a reword therefore splits that snippet's use history — accepted on
// purpose). Body is the expansion text; it may carry {{clip}} (resolved at
// expand time from the X clipboard) and declared {{name}} placeholders
// (resolved from input values collected by the picker form or CLI --set).
type Snippet struct {
	Label       string   `json:"label"`
	Body        string   `json:"body"`
	SearchTerms []string `json:"search_terms"`
	Category    string   `json:"category"`
	Inputs      []Input  `json:"inputs,omitempty"`
}

// HasInputs reports whether the row declares inputs (shorter than nil check at
// every call site).
func (s Snippet) HasInputs() bool { return len(s.Inputs) > 0 }

// DefaultString returns the default value for an input as a string.
func (in *Input) DefaultString() string {
	if in.Default == nil {
		return ""
	}
	if s, ok := in.Default.(string); ok {
		return s
	}
	return ""
}

// inputValidationError is a LOUD validation failure naming the file, row and field.
func inputValidationError(path, label, detail string) error {
	return fmt.Errorf("snip: %s: row %q: inputs: %s", path, label, detail)
}

// validateInputs checks a row's inputs array and returns every failure.
func validateInputs(path string, s Snippet) error {
	if len(s.Inputs) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for i, in := range s.Inputs {
		idx := fmt.Sprintf("[%d]", i)
		if in.Name == "" {
			return inputValidationError(path, s.Label, idx+" name is empty")
		}
		if in.Name == "clip" {
			return inputValidationError(path, s.Label, idx+` name "clip" is reserved`)
		}
		if !InputNamePattern.MatchString(in.Name) {
			return inputValidationError(path, s.Label, fmt.Sprintf("%s name %q does not match %s", idx, in.Name, InputNamePattern))
		}
		if seen[in.Name] {
			return inputValidationError(path, s.Label, idx+` duplicate name "`+in.Name+`"`)
		}
		seen[in.Name] = true
		switch in.Type {
		case "text":
			// default is a string (or nil); required defaults to true
		case "select":
			if len(in.Options) == 0 {
				return inputValidationError(path, s.Label, idx+" select has no options")
			}
			if err := validateOptions(in.Options); err != nil {
				return inputValidationError(path, s.Label, idx+" "+err.Error())
			}
			if d := in.DefaultString(); d != "" {
				if !inOption(d, in.Options) {
					return inputValidationError(path, s.Label, fmt.Sprintf("%s default %q is not in options", idx, d))
				}
			}
		case "multiselect":
			if len(in.Options) == 0 {
				return inputValidationError(path, s.Label, idx+" multiselect has no options")
			}
			if err := validateOptions(in.Options); err != nil {
				return inputValidationError(path, s.Label, idx+" "+err.Error())
			}
			// Default for multiselect is []any from JSON.
			if in.Default != nil {
				switch ds := in.Default.(type) {
				case []interface{}:
					for _, v := range ds {
						if sv, ok := v.(string); ok {
							if !inOption(sv, in.Options) {
								return inputValidationError(path, s.Label,
									fmt.Sprintf("%s default element %q is not in options", idx, sv))
							}
						}
					}
				}
			}
		default:
			return inputValidationError(path, s.Label, idx+` unknown type "`+in.Type+`"`)
		}
	}
	// Two-direction check: every {name}} in body is declared, and every declared input appears.
	return validateBodyPlaceholders(path, s)
}

func validateOptions(opts []string) error {
	if len(opts) == 0 {
		return fmt.Errorf("options is empty")
	}
	seenOpt := make(map[string]bool)
	for _, o := range opts {
		if o == "" {
			return fmt.Errorf("option is empty")
		}
		if seenOpt[o] {
			return fmt.Errorf("duplicate option %q", o)
		}
		seenOpt[o] = true
	}
	return nil
}

func inOption(v string, opts []string) bool {
	for _, o := range opts {
		if o == v {
			return true
		}
	}
	return false
}

var placeholderRE = regexp.MustCompile(`{{\w+}}`)

func validateBodyPlaceholders(path string, s Snippet) error {
	names := make(map[string]bool)
	for _, in := range s.Inputs {
		names[in.Name] = true
	}
	matches := placeholderRE.FindAllString(s.Body, -1)
	for _, m := range matches {
		// strip {{ and }}
		name := m[2 : len(m)-2]
		if name == "clip" {
			continue // {{clip}} is handled separately, not an input
		}
		if !names[name] {
			return inputValidationError(path, s.Label,
				fmt.Sprintf("body references undeclared input %q (did you forget to declare it in inputs?)", name))
		}
	}
	// Reverse: every declared input must appear in the body.
	for _, in := range s.Inputs {
		if !strings.Contains(s.Body, "{{"+in.Name+"}}") {
			return inputValidationError(path, s.Label,
				fmt.Sprintf("declared input %q never appears in the body", in.Name))
		}
	}
	return nil
}

// Path returns the artifact location: $SNIP_CONFIG overrides (tests), else
// $XDG_CONFIG_HOME/snip/snippets.json, else ~/.config/snip/snippets.json —
// the same XDG resolution home-manager's xdg.configFile writes to.
func Path() (string, error) {
	if p := os.Getenv("SNIP_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("snip: resolve home for the artifact path: %w", err)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "snip", "snippets.json"), nil
	}
	return filepath.Join(home, ".config", "snip", "snippets.json"), nil
}

// Load reads + validates the artifact. Validation is LOUD and names the file:
// a broken artifact must say which file is broken, because the tool is
// otherwise unusable (the picker would list nothing and every paste would
// fail) — silent-empty is the failure mode that looks like "the tool is dead".
//
//   - the file must parse as a JSON array;
//   - every label must be non-empty;
//   - labels must be UNIQUE (a duplicate is an editing accident in the nix
//     list, and the label is now the identity the matcher/telemetry key on);
//   - when a row declares inputs, every input is validated (type, name, options,
//     defaults) and body placeholders are checked in both directions.
func Load(path string) ([]Snippet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snip: %s: %w", path, err)
	}
	var out []Snippet
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("snip: %s: not a valid snippets.json array: %v", path, err)
	}
	seen := make(map[string]struct{}, len(out))
	for i, s := range out {
		if s.Label == "" {
			return nil, fmt.Errorf("snip: %s: entry %d has an empty label", path, i)
		}
		if _, dup := seen[s.Label]; dup {
			return nil, fmt.Errorf("snip: %s: duplicate label %q", path, s.Label)
		}
		seen[s.Label] = struct{}{}
		if err := validateInputs(path, s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Find returns the snippet with exactly this label, and whether it was found.
// Label matching is EXACT (byte equality) — the label is the row's identity,
// passed verbatim from the picker/CLI, never guessed.
func Find(snippets []Snippet, label string) (Snippet, bool) {
	for _, s := range snippets {
		if s.Label == label {
			return s, true
		}
	}
	return Snippet{}, false
}
