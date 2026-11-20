// Package form resolves declared, typed inputs on snippet rows.
// It is a small pure-function package: Validate checks the two-direction
// placeholder contract (reusable by the loader and tests), and Apply
// substitutes collected values into a body.
package form

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ZacxDev/promptshipper/desktop/snip/internal/artifact"
)

var placeholderRE = regexp.MustCompile(`{{\w+}}`)

// Validate checks a body against its declared inputs in both directions:
//  1. Every {{name}} in body that is not {{clip}} must be a declared input.
//  2. Every declared input must appear in the body.
//
// It duplicates the loader's check (artifact.validateBodyPlaceholders) so
// callers outside the package (tests, the form package itself) can call it
// independently.
func Validate(body string, inputs []artifact.Input) error {
	names := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		names[in.Name] = true
	}
	matches := placeholderRE.FindAllString(body, -1)
	for _, m := range matches {
		name := m[2 : len(m)-2]
		if name == "clip" {
			continue
		}
		if !names[name] {
			return fmt.Errorf("body references undeclared input %q", name)
		}
	}
	for _, in := range inputs {
		if !strings.Contains(body, "{{"+in.Name+"}}") {
			return fmt.Errorf("declared input %q never appears in the body", in.Name)
		}
	}
	return nil
}

// Apply substitutes input values into body. ClipVar ({{clip}}) is NOT resolved
// here — the paste helper resolves it first, then calls Apply, so a typed value
// containing the literal text "{{clip}}" stays literal.
//
// Rules:
//   - Required inputs whose value is empty-after-TrimSpace produce an error
//     naming the input.
//   - Unknown keys in values are an error naming them.
//   - multiselect values are joined with the input's Join delimiter (default ", ").
//   - After substitution, any remaining {{name}} placeholder is an error.
func Apply(body string, inputs []artifact.Input, values map[string]string) (string, error) {
	result := body

	// Validate: no unknown keys.
	for k := range values {
		if !hasInput(inputs, k) {
			return "", fmt.Errorf("unknown input value %q", k)
		}
	}

	for _, in := range inputs {
		v, ok := values[in.Name]
		if !ok {
			v = ""
		}
		// For multiselect: use join; empty is legal.
		// For text/select: substitute directly.
		// We store all values as strings in the map (multiselect pre-joined by the picker/CLI).
		if in.IsRequired() && strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("required input %q is empty", in.Name)
		}
		result = strings.ReplaceAll(result, "{{"+in.Name+"}}", v)
	}

	// After substitution, refuse if any declared placeholder is still present.
	for _, in := range inputs {
		if strings.Contains(result, "{{"+in.Name+"}}") {
			return "", fmt.Errorf("input %q still unresolved after substitution", in.Name)
		}
	}
	return result, nil
}

func hasInput(inputs []artifact.Input, name string) bool {
	for _, in := range inputs {
		if in.Name == name {
			return true
		}
	}
	return false
}

// PromptFor returns the display prompt for an input: the Prompt field if set,
// else the Name.
func PromptFor(in artifact.Input) string {
	if in.Prompt != "" {
		return in.Prompt
	}
	return in.Name
}

// JoinFor returns the join string for a multiselect input, defaulting to ", ".
func JoinFor(in artifact.Input) string {
	if in.Join != "" {
		return in.Join
	}
	return ", "
}

// MultiDefault returns the default values for a multiselect as a []string.
func MultiDefault(in artifact.Input) []string {
	if in.Default == nil {
		return nil
	}
	switch ds := in.Default.(type) {
	case []interface{}:
		out := make([]string, 0, len(ds))
		for _, v := range ds {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// EffectiveDefault returns the effective default value for an input as a string.
// For text: Default as string or "".
// For select: Default as string or first option.
// For multiselect: pre-joined default values or "".
func EffectiveDefault(in artifact.Input) string {
	switch in.Type {
	case "text":
		return in.DefaultString()
	case "select":
		d := in.DefaultString()
		if d != "" {
			return d
		}
		if len(in.Options) > 0 {
			return in.Options[0]
		}
		return ""
	case "multiselect":
		defs := MultiDefault(in)
		if len(defs) == 0 {
			return ""
		}
		return strings.Join(defs, JoinFor(in))
	}
	return ""
}
