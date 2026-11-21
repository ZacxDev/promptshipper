package picker

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// 🔴 THE KEY STRINGS ARE PINNED BECAUSE A CHORD THAT DOES NOT MATCH THE STRING
// ITS OWN KEY TABLE COMPARES IS DEAD — silently, at exit 0, with the operator's
// keystroke doing nothing at all. `space` shipped exactly that way (a
// `case " "` against a `String()` of "space"), so the mapping is asserted in
// BOTH directions here rather than inferred from the decoder's source.
func TestWordDeleteKeyStringsMatchWhatTheTablesCompare(t *testing.T) {
	// What the operator's terminal sends for Ctrl+Backspace: 0x17, which
	// ultraviolet decodes to Code 'w' + ModCtrl with NO Text.
	if got := ctrl('w').String(); got != WordDeleteKey {
		t.Fatalf("the Ctrl+Backspace keystroke's String() = %q; every text key "+
			"table compares %q, so the binding would never fire", got, WordDeleteKey)
	}
	// The extended-keyboard-protocol spelling of the same keystroke.
	if got := (tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModCtrl}).String(); got != WordDeleteAltKey {
		t.Fatalf("ctrl+backspace String() = %q, want %q", got, WordDeleteAltKey)
	}
	// NEGATIVE CONTROL — THE ONE THAT MATTERS. A key that CARRIES Text reports
	// the TEXT (Key.String returns Text whenever it is non-empty and not a bare
	// space), so a test built this way asserts against "w" and passes while
	// measuring a key no table ever sees. That is precisely the shape that made
	// the space bug invisible, so it is pinned here rather than left implied:
	// if this ever starts reporting the chord, the two pins above stop
	// discriminating and the control must be rebuilt.
	if got := (tea.KeyPressMsg{Code: 'w', Text: "w", Mod: tea.ModCtrl}).String(); got == WordDeleteKey {
		t.Fatalf("a Text-carrying key reported %q — the control cannot fire, so "+
			"the pins above prove nothing", WordDeleteKey)
	}
}

func TestDeleteWordBackward(t *testing.T) {
	for _, tc := range []struct {
		in, want, why string
	}{
		{"", "", "empty box: nothing to delete, and no panic"},
		{"   ", "", "whitespace only: the whole line goes"},
		{"foo", "", "one bare word"},
		{"foo bar", "foo ", "the trailing word AND its separator go"},
		{"foo bar ", "foo ", "ONE press: the spaces first, THEN the word"},
		{"foo bar   ", "foo ", "…however many trailing spaces there are"},
		{"   foo", "   ", "leading whitespace is not the word — it stays"},
		{"a\tb", "a\t", "a tab is a boundary too"},
		{"日本語 テスト", "日本語 ", "multi-byte: rune-wise, not byte-wise"},
	} {
		if got := deleteWordBackward(tc.in); got != tc.want {
			t.Errorf("deleteWordBackward(%q) = %q, want %q (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

func TestDeleteRuneBackward(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"a", ""},
		{"ab", "a"},
		{"a日", "a"},
		{"日本", "日"},
	} {
		if got := deleteRuneBackward(tc.in); got != tc.want {
			t.Errorf("deleteRuneBackward(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
