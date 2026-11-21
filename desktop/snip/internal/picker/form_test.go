package picker

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ZacxDev/promptshipper/desktop/snip/internal/artifact"
	"github.com/ZacxDev/promptshipper/desktop/snip/internal/shared"
	"github.com/ZacxDev/promptshipper/desktop/snip/internal/values"
)

// ptrBool is the explicit required flag for a fixture input.
func ptrBool(b bool) *bool { return &b }

// formCatalog is the form fixture: row 0 declares one input of each type, row 1
// declares none. The scopes default is deliberately a SINGLE option ("api") so a
// remembered multiselect value ("api, web") is distinguishable from it.
var formCatalog = []artifact.Snippet{
	{
		Label:    "Deploy",
		Body:     "Deploy {{service}} to {{env}}, then verify {{scopes}}.",
		Category: "prompt",
		Inputs: []artifact.Input{
			{Name: "service", Type: "text", Prompt: "Service", Required: ptrBool(true)},
			{Name: "env", Type: "select", Prompt: "Environment", Options: []string{"staging", "prod"}, Default: "staging"},
			{Name: "scopes", Type: "multiselect", Prompt: "Scopes", Options: []string{"api", "web", "db"}, Default: []any{"api"}, Join: ", "},
		},
	},
	{Label: "plain row", Body: "nothing to fill", Category: "prompt"},
}

// formKeys are the key messages the form's state machine reads, built the way
// bubbletea reports them (a real space is Code=KeySpace/Text=" ", which
// String()s to "space").
func keySpace() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "} }
func keyTab() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyTab} }
func keyShiftTab() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
}
func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

// enterForm drives Enter from the browse list on a row with inputs, which must
// enter the form and spawn nothing.
func enterForm(t *testing.T, fx *fakeLauncher) Model {
	t.Helper()
	// Isolate the remembered-values store so the prefill is deterministic.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := New(formCatalog, fx)
	if len(m.filtered) == 0 || !m.filtered[0].HasInputs() {
		t.Fatalf("fixture: the first row must declare inputs")
	}
	next, intents := m.Step(keyEnter())
	if next.mode != modeForm {
		t.Fatalf("Enter on a row with inputs: mode = %v, want form", next.mode)
	}
	if len(intents) != 0 {
		t.Fatalf("entering the form must spawn nothing; intents = %+v", intents)
	}
	if len(fx.spawned) != 0 {
		t.Fatalf("entering the form must spawn nothing; spawned = %v", fx.spawned)
	}
	return next
}

// Enter on a row that declares inputs opens the form instead of pasting.
func TestFormIsEnteredFromARowWithInputs(t *testing.T) {
	fx := &fakeLauncher{}
	m := enterForm(t, fx)
	if m.formSnippet.Label != "Deploy" {
		t.Fatalf("formSnippet = %q, want the row under the cursor", m.formSnippet.Label)
	}
	// The declared defaults are the starting values.
	if m.formValues["env"] != "staging" || m.formValues["scopes"] != "api" {
		t.Fatalf("initial values = %v, want the declared defaults", m.formValues)
	}
}

// tab/down move forward, shift+tab/up move back, clamped at the ends.
func TestFormTabAndShiftTabMoveBetweenFields(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	check := func(want int) {
		t.Helper()
		if m.formCursor != want {
			t.Fatalf("formCursor = %d, want %d", m.formCursor, want)
		}
	}
	check(0)
	m = upd(m, keyTab())
	check(1)
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyDown})
	check(2)
	m = upd(m, keyTab())
	check(2) // clamped at the last field
	m = upd(m, keyShiftTab())
	check(1)
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyUp})
	check(0)
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyUp})
	check(0) // clamped at the first field
}

// left/right move WITHIN a select and COMMIT the highlighted option: a select
// has no multi semantics, so highlight == choice ("left/right picks and the
// choice is shown inline"). Watched RED before the fix, where movement only
// moved the highlight and the value stayed on the default.
func TestFormLeftRightPicksASelect(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	m = upd(m, keyTab()) // focus env (select); default staging, highlight index 0
	if m.formValues["env"] != "staging" {
		t.Fatalf("initial env = %q, want staging", m.formValues["env"])
	}
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.formValues["env"] != "prod" {
		t.Fatalf("right on a select did not commit the highlighted option: env = %q, want prod", m.formValues["env"])
	}
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.formValues["env"] != "staging" {
		t.Fatalf("left on a select did not commit the highlighted option: env = %q, want staging", m.formValues["env"])
	}
}

// left/right move the highlight WITHIN a multiselect but do NOT change the
// value: only space toggles.
func TestFormLeftRightMovesWithinMultiselectWithoutChangingIt(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	m = upd(m, keyTab())
	m = upd(m, keyTab()) // focus scopes (multiselect); default "api"
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.formOptionCursor != 1 {
		t.Fatalf("option cursor = %d, want 1 after right", m.formOptionCursor)
	}
	if m.formValues["scopes"] != "api" {
		t.Fatalf("right must not change a multiselect value: scopes = %q, want api", m.formValues["scopes"])
	}
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.formOptionCursor != 0 {
		t.Fatalf("option cursor = %d, want 0 after left", m.formOptionCursor)
	}
}

// space toggles a multiselect option. Watched RED before the fix, where a real
// space (String() == "space") never matched the form's key table.
func TestFormSpaceTogglesMultiselect(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	m = upd(m, keyTab())
	m = upd(m, keyTab()) // focus scopes; default "api", highlight index 0
	m = upd(m, keySpace())
	if m.formValues["scopes"] != "" {
		t.Fatalf("space on the selected option must toggle it off: scopes = %q, want empty", m.formValues["scopes"])
	}
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyRight}) // highlight web
	m = upd(m, keySpace())
	if m.formValues["scopes"] != "web" {
		t.Fatalf("space must toggle the highlighted option on: scopes = %q, want web", m.formValues["scopes"])
	}
}

// space commits the highlighted option on a select too.
func TestFormSpaceCommitsSelect(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	m = upd(m, keyTab())                            // focus env
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyRight}) // highlight prod
	m = upd(m, keySpace())
	if m.formValues["env"] != "prod" {
		t.Fatalf("space on a select must commit the highlighted option: env = %q, want prod", m.formValues["env"])
	}
}

// A space typed into a TEXT field is ordinary text, not a toggle — the form
// must keep accepting multi-word values.
func TestFormSpaceIsLiteralInATextField(t *testing.T) {
	m := enterForm(t, &fakeLauncher{})
	m = typeStr(m, "a b") // focused field starts on service (text)
	if m.formValues["service"] != "a b" {
		t.Fatalf("a space in a text field must be literal: service = %q, want %q", m.formValues["service"], "a b")
	}
}

// Enter on an empty REQUIRED field stays on the form, names that field's prompt
// in the notice, and returns no FormSubmit and spawns nothing.
func TestFormEnterOnEmptyRequiredStaysAndNamesTheField(t *testing.T) {
	captured := capturePickerRows(t)
	fx := &fakeLauncher{}
	m := enterForm(t, fx) // service is empty and required
	next, intents := m.Step(keyEnter())
	if next.mode != modeForm {
		t.Fatalf("Enter on an empty required field must stay on the form: mode = %v", next.mode)
	}
	if len(intents) != 0 {
		t.Fatalf("intents = %+v, want none", intents)
	}
	if next.Selected != "" {
		t.Fatalf("Selected = %q, must not be set on a blocked submit", next.Selected)
	}
	if !strings.Contains(next.notice, "Service") {
		t.Fatalf("notice = %q, want it to name the field's prompt %q", next.notice, "Service")
	}
	if len(fx.spawned) != 0 {
		t.Fatalf("a blocked submit must spawn nothing: %v", fx.spawned)
	}
	if len(captured.rows) != 0 {
		t.Fatalf("a blocked submit must emit no row: %+v", captured.rows)
	}
}

// Enter when satisfied returns EXACTLY ONE FormSubmit carrying the collected
// values, and through Update spawns EXACTLY ONE paste with those --set values.
func TestFormEnterSubmitsAndSpawnsExactlyOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fx := &fakeLauncher{}
	m := New(formCatalog, fx)
	m, _ = m.Step(keyEnter()) // enter the form
	m = typeStr(m, "myapp")   // fill the required text field

	next, intents := m.Step(keyEnter())
	if len(intents) != 1 {
		t.Fatalf("intents = %+v, want exactly one FormSubmit", intents)
	}
	fs, ok := intents[0].(FormSubmit)
	if !ok {
		t.Fatalf("intent = %T, want FormSubmit", intents[0])
	}
	if fs.Label != "Deploy" {
		t.Fatalf("FormSubmit.Label = %q, want Deploy", fs.Label)
	}
	want := map[string]string{"service": "myapp", "env": "staging", "scopes": "api"}
	if !reflect.DeepEqual(fs.Values, want) {
		t.Fatalf("FormSubmit.Values = %v, want %v", fs.Values, want)
	}
	if next.Selected != "Deploy" {
		t.Fatalf("Selected = %q, want Deploy", next.Selected)
	}

	// The intent through Update must spawn exactly one paste carrying the values.
	_, cmd := updCmd(m, keyEnter())
	sp := spawnedFrom(runCmd(cmd))
	if sp.Err != nil {
		t.Fatalf("spawn: %v", sp.Err)
	}
	if len(fx.spawned) != 1 || fx.spawned[0] != "Deploy" {
		t.Fatalf("spawned = %v, want exactly [Deploy]", fx.spawned)
	}
	if len(fx.setVals) != 1 {
		t.Fatalf("recorded set values = %v, want exactly one", fx.setVals)
	}
	if !reflect.DeepEqual(fx.setVals[0], want) {
		t.Fatalf("spawn --set = %v, want %v", fx.setVals[0], want)
	}
}

// esc in the form returns to the browse LIST — not out of the picker — with no
// paste and no fire row.
func TestFormEscReturnsToBrowseAndPastesNothing(t *testing.T) {
	captured := capturePickerRows(t)
	fx := &fakeLauncher{}
	m := enterForm(t, fx)
	next, intents := m.Step(tea.KeyPressMsg{Code: tea.KeyEscape})
	if next.mode != modeBrowse {
		t.Fatalf("esc: mode = %v, want browse (the form returns to the list)", next.mode)
	}
	if next.quit || next.Canceled {
		t.Fatalf("esc in the form must not quit the picker: quit=%v canceled=%v", next.quit, next.Canceled)
	}
	if len(intents) != 0 {
		t.Fatalf("esc intents = %+v, want none", intents)
	}
	if len(fx.spawned) != 0 {
		t.Fatalf("esc must paste nothing: %v", fx.spawned)
	}
	if len(captured.rows) != 0 {
		t.Fatalf("esc must emit no fire row: %+v", captured.rows)
	}
}

// Enter on a row that declares NO inputs still pastes directly and never shows
// the form.
func TestRowWithoutInputsPastesDirectlyNeverTheForm(t *testing.T) {
	fx := &fakeLauncher{}
	m := New(formCatalog, fx)
	m = upd(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (the plain row)", m.cursor)
	}
	next, intents := m.Step(keyEnter())
	if next.mode == modeForm {
		t.Fatal("a row without inputs must never show the form")
	}
	if len(intents) != 1 {
		t.Fatalf("intents = %+v, want exactly one Select", intents)
	}
	if _, ok := intents[0].(Select); !ok {
		t.Fatalf("intent = %T, want Select", intents[0])
	}
	_, cmd := updCmd(m, keyEnter())
	_ = spawnedFrom(runCmd(cmd))
	if len(fx.spawned) != 1 || fx.spawned[0] != "plain row" {
		t.Fatalf("spawned = %v, want exactly [plain row]", fx.spawned)
	}
}

// The form is entered ONLY from the browse list. In the shared-clipboard list
// Enter picks the CLIP even when a catalog row with inputs exists.
func TestFormIsNeverEnteredFromTheSharedClipboardList(t *testing.T) {
	fx := &fakeLauncher{clips: []shared.Clip{{ID: 7, Body: "a clip body", Device: "phone"}}}
	m := New(formCatalog, fx) // the catalog DOES carry a row with inputs
	m, cmd := updCmd(m, ctrl('g'))
	for _, msg := range runCmd(cmd) {
		m = upd(m, msg)
	}
	if m.mode != modeShared {
		t.Fatalf("mode = %v, want shared", m.mode)
	}
	next, intents := m.Step(keyEnter())
	if next.mode != modeShared {
		t.Fatalf("mode = %v, want shared (the form is browse-only)", next.mode)
	}
	if len(intents) != 1 {
		t.Fatalf("intents = %+v, want exactly one SelectClip", intents)
	}
	if _, ok := intents[0].(SelectClip); !ok {
		t.Fatalf("intent = %T, want SelectClip — never a form entry", intents[0])
	}
}

// 🔴 DEFECT 1 REGRESSION. A remembered value must win for EVERY input type.
// Watched RED before the fix: the remembered MULTISELECT value ("api, web") was
// overwritten by the declared default ("api").
func TestPrefillRememberedMultiselectWinsOverTheDefault(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path, err := values.Path()
	if err != nil {
		t.Fatal(err)
	}
	vf := &values.File{}
	vf.Set("Deploy", "scopes", "api, web", 1)            // remembered multiselect
	vf.Set("Deploy", "service", "remembered-service", 2) // remembered text
	if err := values.Save(path, vf); err != nil {
		t.Fatal(err)
	}

	fx := &fakeLauncher{}
	m := New(formCatalog, fx)
	m, _ = m.Step(keyEnter())
	if m.mode != modeForm {
		t.Fatalf("mode = %v, want form", m.mode)
	}
	if got := m.formValues["scopes"]; got != "api, web" {
		t.Fatalf("remembered multiselect value was discarded: scopes = %q, want %q "+
			"(the declared default is %q); the remembered value must win for every type",
			got, "api, web", "api")
	}
	if got := m.formValues["service"]; got != "remembered-service" {
		t.Fatalf("remembered text value was discarded: service = %q, want remembered-service", got)
	}
}

// A remembered SELECT value no longer in options is dropped, falling back to the
// input's effective default (the declared default, else the first option).
func TestPrefillDropsARememberedSelectNoLongerInOptions(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path, err := values.Path()
	if err != nil {
		t.Fatal(err)
	}
	vf := &values.File{}
	vf.Set("Deploy", "env", "retired-env", 1)
	if err := values.Save(path, vf); err != nil {
		t.Fatal(err)
	}

	m := New(formCatalog, &fakeLauncher{})
	m, _ = m.Step(keyEnter())
	if got := m.formValues["env"]; got != "staging" {
		t.Fatalf("env = %q, want the declared default staging (a remembered value not in options is dropped)", got)
	}
}

// 🔴 DEFECT 2 REGRESSION. A ctrl+e edit paste must carry the row's input values,
// or the helper's required-input pre-check refuses the paste (exit 1) after the
// edit has already landed. Watched RED before the fix, where the edit spawn
// passed a nil map.
func TestEditSpawnCarriesTheRowsInputValues(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cat := []artifact.Snippet{{
		Label: "probe inputs row",
		Body:  "Walk {{target}} as a user.",
		Inputs: []artifact.Input{
			{Name: "target", Type: "text", Prompt: "Target", Default: "docs/ui-ledger.md", Required: ptrBool(true)},
		},
	}}
	fx := &fakeLauncher{readBody: "Walk {{target}} as a user, please."}
	m := New(cat, fx)
	m, cmd := updCmd(m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = upd(m, stagedFrom(runCmd(cmd)))
	m, landCmd := updCmd(m, editorDone{})
	er := editReadFrom(t, runCmd(landCmd))
	if !er.Ready || er.Err != nil {
		t.Fatalf("editRead = %+v, want Ready", er)
	}

	_, spawnCmd := updCmd(m, er)
	_ = spawnedFrom(runCmd(spawnCmd))
	if len(fx.setVals) != 1 {
		t.Fatalf("recorded set values = %v, want exactly one", fx.setVals)
	}
	if got := fx.setVals[0]["target"]; got != "docs/ui-ledger.md" {
		t.Fatalf("edit spawn --set = %v, want target=docs/ui-ledger.md (the row's prefilled value); "+
			"a nil/empty map makes the helper's required-input pre-check refuse the paste", fx.setVals[0])
	}
}

// A row with NO inputs keeps spawning the edit paste with nil values, exactly as
// before — the carrying is scoped to rows that declare inputs.
func TestEditSpawnWithNoInputsPassesNilValues(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fx := &fakeLauncher{readBody: "a NEW body"}
	m := New(editCatalog, fx)
	m, cmd := updCmd(m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = upd(m, stagedFrom(runCmd(cmd)))
	m, landCmd := updCmd(m, editorDone{})
	er := editReadFrom(t, runCmd(landCmd))
	if !er.Ready {
		t.Fatalf("editRead = %+v, want Ready", er)
	}
	_, spawnCmd := updCmd(m, er)
	_ = spawnedFrom(runCmd(spawnCmd))
	if len(fx.setVals) != 1 {
		t.Fatalf("recorded set values = %v, want exactly one", fx.setVals)
	}
	if fx.setVals[0] != nil {
		t.Fatalf("a row with no inputs must pass nil values, got %v", fx.setVals[0])
	}
}

// ctrl+w deletes the previous WORD in the form's TEXT field — the chord the
// terminal sends for Ctrl+Backspace (WordDeleteKey). Both spellings are driven,
// because only ONE of them reaches this operator's terminal.
func TestFormCtrlWDeletesWordBackwardInTextField(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		ctrl('w'),
		{Code: tea.KeyBackspace, Mod: tea.ModCtrl},
	} {
		fx := &fakeLauncher{}
		m := enterForm(t, fx)
		m = typeStr(m, "api gateway")
		if got := m.formValues["service"]; got != "api gateway" {
			t.Fatalf("fixture: text field = %q, want %q", got, "api gateway")
		}
		m = upd(m, key)
		if got := m.formValues["service"]; got != "api " {
			t.Fatalf("%q: text field = %q, want %q", key.String(), got, "api ")
		}
		// The last word goes on the next press, and a further press on the
		// now-empty field is a no-op rather than a panic.
		m = upd(m, key)
		m = upd(m, key)
		if got := m.formValues["service"]; got != "" {
			t.Fatalf("%q: text field = %q, want empty", key.String(), got)
		}
	}
}

// 🔴 AND IT IS INERT ON A SELECT/MULTISELECT — those values are CHOSEN, not
// typed, so there is no word to delete and the chord must not silently rewrite
// one. A blurred TEXT field is left alone here as well: the delete targets the
// focused field, so formCursor decides, not "the last thing typed".
func TestFormCtrlWIsInertOnChosenFields(t *testing.T) {
	fx := &fakeLauncher{}
	m := enterForm(t, fx)
	m = typeStr(m, "svc")
	m = upd(m, keyTab()) // -> env (select)
	m = upd(m, keyTab()) // -> scopes (multiselect)
	if m.formCursor != 2 {
		t.Fatalf("fixture: formCursor = %d, want 2", m.formCursor)
	}
	env, scopes, text := m.formValues["env"], m.formValues["scopes"], m.formValues["service"]
	m = upd(m, ctrl('w'))
	if m.formValues["env"] != env || m.formValues["scopes"] != scopes || m.formValues["service"] != text {
		t.Fatalf("ctrl+w changed a CHOSEN value or a blurred text field: "+
			"env %q->%q, scopes %q->%q, service %q->%q",
			env, m.formValues["env"], scopes, m.formValues["scopes"], text, m.formValues["service"])
	}
}
