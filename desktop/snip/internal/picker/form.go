// The picker's FORM mode: when a row with declared inputs is selected,
// the list is replaced by one line per field so the operator can fill values
// before the paste is spawned.
//
// 🔴 STRUCTURALLY A MODE, like shared: esc returns to the LIST, not out of
// the picker; the key table is disjoint; nothing falls through to browse.
package picker

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ZacxDev/promptshipper/desktop/snip/internal/artifact"
	"github.com/ZacxDev/promptshipper/desktop/snip/internal/form"
	"github.com/ZacxDev/promptshipper/desktop/snip/internal/ui"
	"github.com/ZacxDev/promptshipper/desktop/snip/internal/values"
)

// prefillFormValues returns the initial values map for a snippet's inputs:
// the row's DECLARED default for each input, overridden by a remembered value
// when one exists. A remembered `select` value that is no longer among the
// input's options is dropped, so the declared default stands.
//
// 🔴 THE REMEMBERED VALUE WINS FOR EVERY TYPE. The declared default is only a
// fallback; it must not overwrite a remembered value.
func (m *Model) prefillFormValues(s artifact.Snippet) map[string]string {
	vals := make(map[string]string, len(s.Inputs))
	var vf *values.File
	if path, err := values.Path(); err == nil {
		vf = values.Load(path)
	}
	for _, in := range s.Inputs {
		v := form.EffectiveDefault(in)
		if vf != nil {
			if rv, _, ok := vf.Get(s.Label, in.Name); ok {
				if in.Type == "select" {
					if inOptionVal(rv, in.Options) {
						v = rv
					} else {
						vf.Remove(s.Label, in.Name)
					}
				} else {
					v = rv
				}
			}
		}
		vals[in.Name] = v
	}
	return vals
}

func inOptionVal(v string, opts []string) bool {
	for _, o := range opts {
		if o == v {
			return true
		}
	}
	return false
}

// stepForm is the form mode's whole key table. 🔴 DISJOINT: every key it does
// not name is swallowed.
func (m Model) stepForm(msg tea.KeyPressMsg) (Model, []Intent) {
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.query = ""
		m.cursor = 0
		m.notice = ""
		m.refilter()
		return m, nil
	case "ctrl+c":
		m.Canceled, m.quit = true, true
		return m, nil
	case "enter", "return":
		if m.formSnippet.HasInputs() {
			for _, in := range m.formSnippet.Inputs {
				if in.IsRequired() {
					v := m.formValues[in.Name]
					if strings.TrimSpace(v) == "" {
						m.notice = fmt.Sprintf("%s is required", form.PromptFor(in))
						return m, nil
					}
				}
			}
		}
		m.Selected = m.formSnippet.Label
		m.rememberFormValues()
		return m, []Intent{FormSubmit{Label: m.formSnippet.Label, Values: m.formValues}}
	case "tab", "down":
		if m.formCursor < len(m.formSnippet.Inputs)-1 {
			m.formCursor++
		}
		m.formOptionCursor = 0
		return m, nil
	case "shift+tab", "up":
		if m.formCursor > 0 {
			m.formCursor--
		}
		m.formOptionCursor = 0
		return m, nil
	case "left":
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "select" || in.Type == "multiselect" {
				if m.formOptionCursor > 0 {
					m.formOptionCursor--
				}
				// 🔴 ON A SELECT, MOVING IS CHOOSING. A select has no multi
				// semantics, so the highlighted option IS the value — left/right
				// commits it (brief: "left/right picks and the choice is shown
				// inline"). A multiselect only moves the highlight; space toggles.
				if in.Type == "select" && m.formOptionCursor < len(in.Options) {
					m.formValues[in.Name] = in.Options[m.formOptionCursor]
				}
			}
		}
		return m, nil
	case "right":
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "select" || in.Type == "multiselect" {
				if m.formOptionCursor < len(in.Options)-1 {
					m.formOptionCursor++
				}
				if in.Type == "select" && m.formOptionCursor < len(in.Options) {
					m.formValues[in.Name] = in.Options[m.formOptionCursor]
				}
			}
		}
		return m, nil
	case "space":
		// 🔴 THE KEY IS "space", NOT " ". bubbletea reports a real space as
		// Code=KeySpace/Text=" ", whose String() is "space" (Text==" " is the one
		// value String() does not return); a case on " " never matched, so the
		// toggle was dead. Space toggles a multiselect and commits a select; a
		// space in a text field falls through to the typed-text arm below.
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "select" {
				if m.formOptionCursor < len(in.Options) {
					m.formValues[in.Name] = in.Options[m.formOptionCursor]
				}
				return m, nil
			}
			if in.Type == "multiselect" {
				if m.formOptionCursor < len(in.Options) {
					m.formValues[in.Name] = toggleMultiOpt(m.formValues[in.Name], in.Options[m.formOptionCursor], form.JoinFor(in))
				}
				return m, nil
			}
		}
	case "backspace":
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "text" {
				v := m.formValues[in.Name]
				if r := []rune(v); len(r) > 0 {
					m.formValues[in.Name] = string(r[:len(r)-1])
				}
			}
		}
		return m, nil
	case "ctrl+u":
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "text" {
				m.formValues[in.Name] = ""
			}
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod == 0 {
		if m.formCursor < len(m.formSnippet.Inputs) {
			in := m.formSnippet.Inputs[m.formCursor]
			if in.Type == "text" {
				m.formValues[in.Name] += msg.Text
			}
		}
	}
	return m, nil
}

func toggleMultiOpt(current, opt, join string) string {
	if current == "" {
		return opt
	}
	parts := strings.Split(current, join)
	for i, p := range parts {
		if strings.TrimSpace(p) == strings.TrimSpace(opt) {
			newParts := append(parts[:i], parts[i+1:]...)
			return strings.Join(newParts, join)
		}
	}
	return current + join + opt
}

func (m *Model) rememberFormValues() {
	path, err := values.Path()
	if err != nil {
		return
	}
	vf := values.Load(path)
	now := time.Now().UnixMilli()
	for _, in := range m.formSnippet.Inputs {
		vf.Set(m.formSnippet.Label, in.Name, m.formValues[in.Name], now)
	}
	_ = values.Save(path, vf)
}

// --- form rendering -----------------------------------------------------------

func (m Model) formRowLine(i int) string {
	if i >= len(m.formSnippet.Inputs) {
		return ""
	}
	in := m.formSnippet.Inputs[i]
	selected := i == m.formCursor

	prompt := form.PromptFor(in)
	prefix := "  "
	if selected {
		prefix = ui.StyPointer.Render("▍") + " "
	}
	promptStyle := ui.StyDim
	if selected {
		promptStyle = ui.StyCursor
	}

	switch in.Type {
	case "text":
		val := m.formValues[in.Name]
		valStr := val
		if selected {
			valStr = val + "█"
		}
		line := prefix + promptStyle.Render(prompt+": ") + valStr
		if in.IsRequired() {
			line += ui.StyDim.Render(" *")
		}
		return truncate(line, m.Width)

	case "select":
		cur := m.formValues[in.Name]
		opts := renderOptionList(in.Options, m.formOptionCursor, selected)
		line := prefix + promptStyle.Render(prompt+": ") + cur + "  " + opts
		return truncate(line, m.Width)

	case "multiselect":
		cur := m.formValues[in.Name]
		opts := renderOptionList(in.Options, m.formOptionCursor, selected)
		line := prefix + promptStyle.Render(prompt+": ") + cur + "  " + opts
		return truncate(line, m.Width)
	}
	return ""
}

func renderOptionList(opts []string, cursor int, focused bool) string {
	if len(opts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, o := range opts {
		if i > 0 {
			b.WriteString(" ")
		}
		if focused && i == cursor {
			b.WriteString(ui.StyCursor.Render(o))
		} else {
			b.WriteString(ui.StyDim.Render(o))
		}
	}
	return b.String()
}

func (m Model) formFooter() string {
	d := ui.StyDim
	sep := d.Render("  ·  ")
	parts := []string{
		ui.StyKey.Render("tab/↑/↓") + d.Render(" field"),
		ui.StyKey.Render("←/→") + d.Render(" option"),
		ui.StyKey.Render("space") + d.Render(" toggle"),
		ui.StyKey.Render("⏎") + d.Render(" submit"),
		ui.StyKey.Render("ctrl+u") + d.Render(" clear"),
		ui.StyKey.Render("esc") + d.Render(" back"),
		d.Render(fmt.Sprintf("%d fields", len(m.formSnippet.Inputs))),
	}
	return joinToWidth(parts, sep, m.Width)
}

func (m Model) formHeader() string {
	const title = " snip form "
	rest := m.formSnippet.Label
	return ui.StyTitle.Render(title) + ui.StyDim.Render(truncate(rest, m.Width-lipgloss.Width(title)))
}
