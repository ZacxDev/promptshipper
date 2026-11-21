package picker

import "unicode"

// ─────────────────────────────────────────────────────────────────────────────
// TEXT-BOX EDITING PRIMITIVES — one copy, three callers.
//
// 🔴 THE THREE TEXT INPUTS ARE ONE WIDGET WITH THREE KEY TABLES. The browse
// filter (picker.go's Step), the shared clipboard filter (shared.go's
// stepShared) and the form's text field (form.go's stepForm) are three separate
// switch statements over the same box: a single line whose cursor is always at
// the END (no arrow-key movement, no mid-string insert). Every editing
// operation below therefore only ever touches the TAIL of the string, and each
// is spelled ONCE here rather than re-derived in each table — the three
// `case "backspace"` arms were byte-identical before this file existed, which
// is the shape that drifts.
// ─────────────────────────────────────────────────────────────────────────────

// WordDeleteKey is the delete-previous-word chord.
//
// 🔴 IT IS ctrl+w BECAUSE THAT IS THE BYTE THE OPERATOR'S TERMINAL ACTUALLY
// SENDS FOR Ctrl+Backspace — measured, not assumed. devrc's Alacritty config
// (`nix/programs/alacritty/default.nix`) binds it, with its own comment naming
// the intent:
//
//	{ key = "Back"; mods = "Control"; chars = "\u0017"; }   # Delete word
//
// 0x17 sits inside ultraviolet's control-code range, so its decoder answers
// `KeyPressEvent{Code: 'w', Mod: ModCtrl}`, whose `String()` is **"ctrl+w"** —
// NOT "ctrl+backspace". A `case "ctrl+backspace"` alone would therefore be DEAD
// on this operator's machine, which is the failure this constant exists to
// avoid. (The sibling binding, Ctrl+Shift+Backspace → "\u0015", is readline's
// kill-to-line-start; form.go's `ctrl+u` arm is its match.)
//
// ⚠ BOTH DIRECTIONS OF THE MAPPING ARE PINNED BY TEST — edit_test.go asserts
// the constructed key events' `String()` values, because a chord that does not
// match the string its own key table compares is exactly the class of bug the
// `space` key shipped with (a `case " "` that could never fire).
const WordDeleteKey = "ctrl+w"

// WordDeleteAltKey is the extended-keyboard-protocol spelling of the SAME
// keystroke. A terminal implementing the Kitty keyboard protocol (or xterm
// `modifyOtherKeys`) reports Backspace+Ctrl as a KEY rather than as 0x17, and
// ultraviolet decodes that to `KeyBackspace` + `ModCtrl` → "ctrl+backspace".
// Bubble Tea v2 pushes both handshakes on its first render, so this spelling is
// reachable; it is matched ALONGSIDE WordDeleteKey rather than instead of it,
// so the binding is live on either class of terminal.
const WordDeleteAltKey = "ctrl+backspace"

// deleteRuneBackward removes the final rune — the plain backspace the box has
// always had. Rune-wise, so a multi-byte character goes whole.
func deleteRuneBackward(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// deleteWordBackward removes everything back to the start of the previous word
// — readline's `unix-word-rubout`, which is what ^W has meant in every text box
// the operator's hands learned it in.
//
// 🔴 THE TRAILING-WHITESPACE PASS IS THE HALF THAT MAKES IT FEEL RIGHT: with
// "foo bar   " the spaces go FIRST and then "bar", so one press yields "foo "
// rather than "foo bar  ". Its absence is not a smaller bug — it makes the
// second press on a spaced-out string appear to do nothing.
//
// Whitespace is `unicode.IsSpace`, not `' '`, so a tab the box somehow holds is
// a boundary too.
func deleteWordBackward(s string) string {
	r := []rune(s)
	i := len(r)
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	return string(r[:i])
}
