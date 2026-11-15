package setup

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// press feeds key names to a model the way bubbletea would.
func press(m *pickerModel, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.Update(msg)
	}
}

const pickerLine = "12:00:02 GET /api/items trace_id=3f9a1c0d [ok]"

func picked(t *testing.T, m *pickerModel) string {
	t.Helper()
	if !m.done || m.result.Kind != PickSelected {
		t.Fatalf("not selected: done=%v result=%+v", m.done, m.result)
	}
	return pickerLine[m.result.Start:m.result.End]
}

// w moves by vim words, so reaching the id takes several, and enter takes
// the ID-like token under the cursor.
func TestPickerWordMotionsAndEnterTakesToken(t *testing.T) {
	m := newPicker("trace id", "?", pickerLine, 3, 50)
	press(m, "W", "W", "W") // WORDs: GET, /api/items, trace_id=3f9a1c0d
	if got := m.runes[m.cur]; got != 't' {
		t.Fatalf("cursor on %q, want start of trace_id=3f9a1c0d", got)
	}
	press(m, "e", "e") // end of trace_id, then the '=' punctuation class
	press(m, "w")      // start of 3f9a1c0d
	press(m, "enter")
	if got := picked(t, m); got != "3f9a1c0d" {
		t.Errorf("picked %q, want 3f9a1c0d", got)
	}
	if m.result.Line != 3 {
		t.Errorf("line = %d, want 3", m.result.Line)
	}
}

func TestPickerVisualSelection(t *testing.T) {
	m := newPicker("trace id", "?", pickerLine, 0, 1)
	press(m, "0", "v", "e", "enter") // select "12"
	if got := picked(t, m); got != "12" {
		t.Errorf("picked %q, want 12", got)
	}
}

func TestPickerNavigationAndSkip(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want PickKind
	}{
		{[]string{"j"}, PickNextLine},
		{[]string{"n"}, PickNextLine},
		{[]string{"k"}, PickPrevLine},
		{[]string{"s"}, PickSkip},
		{[]string{"q"}, PickSkip},
		{[]string{"esc"}, PickSkip},
	} {
		m := newPicker("t", "?", pickerLine, 7, 50)
		press(m, c.keys...)
		if !m.done || m.result.Kind != c.want || m.result.Line != 7 {
			t.Errorf("%v: done=%v result=%+v, want kind %v on line 7", c.keys, m.done, m.result, c.want)
		}
	}
}

// esc first clears a selection; only a second esc skips.
func TestPickerEscClearsSelectionFirst(t *testing.T) {
	m := newPicker("t", "?", pickerLine, 0, 1)
	press(m, "v", "esc")
	if m.done || m.anchor != -1 {
		t.Fatalf("first esc: done=%v anchor=%d, want selection cleared", m.done, m.anchor)
	}
	press(m, "esc")
	if !m.done || m.result.Kind != PickSkip {
		t.Fatalf("second esc: %+v", m.result)
	}
}

func TestPickerEnterOnNonTokenExplains(t *testing.T) {
	m := newPicker("t", "?", "a  b", 0, 1)
	press(m, "l", "enter") // on a space
	if m.done || m.notice == "" {
		t.Errorf("done=%v notice=%q, want a notice and no selection", m.done, m.notice)
	}
}

// Offsets are bytes into the line even with multibyte text before the value.
func TestPickerByteOffsetsWithMultibyte(t *testing.T) {
	line := "héllo wörld id=ab12"
	m := newPicker("t", "?", line, 0, 1)
	press(m, "$", "enter")
	if !m.done || line[m.result.Start:m.result.End] != "ab12" {
		t.Errorf("picked %q", line[m.result.Start:m.result.End])
	}
}

// The picker runs under a real bubbletea program: keys arrive as bytes, the
// model finishes, the program quits, and the result is on the returned model.
func TestPickerUnderRealProgram(t *testing.T) {
	var out strings.Builder
	m := newPicker("trace id", "is it on this line?", pickerLine, 1, 5)
	final, err := tea.NewProgram(m, tea.WithInput(strings.NewReader("0WWWww\r")), tea.WithOutput(&out)).Run()
	if err != nil {
		t.Fatal(err)
	}
	got := final.(*pickerModel)
	if !got.done || got.result.Kind != PickSelected {
		t.Fatalf("result = %+v", got.result)
	}
	if text := pickerLine[got.result.Start:got.result.End]; text != "3f9a1c0d" {
		t.Errorf("picked %q, want the token under the cursor", text)
	}
	if !strings.Contains(out.String(), "line 2/5") {
		t.Errorf("program output never showed the line position: %q", out.String())
	}
}
