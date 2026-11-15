package thread

import (
	"strings"
	"testing"
)

func TestApplyNoteEdits(t *testing.T) {
	base := "## Task\nPlan Lisbon.\n\n## Next steps\nBook the flight."
	edit := func(find, repl string) string {
		return editFind + "\n" + find + "\n" + editSep + "\n" + repl + "\n" + editReplace
	}
	cases := []struct {
		name, notes, update, want, err string
	}{
		{name: "complete notes are not edits", notes: base, update: "  ## Task\nPlan Porto.\n", err: "the answer is not edits"},
		{name: "first notes", notes: "", update: "  ## Task\nPlan Lisbon.\n", want: "## Task\nPlan Lisbon."},
		{name: "empty first notes", notes: "", update: " \n ", err: "the update is empty"},
		{name: "replace", notes: base, update: edit("Book the flight.", "Book the hotel."),
			want: "## Task\nPlan Lisbon.\n\n## Next steps\nBook the hotel."},
		{name: "two blocks with prose around them", notes: base,
			update: "Here are my edits:\n" + edit("Plan Lisbon.", "Plan Lisbon in May.") + "\nand\n" + edit("Book the flight.", "Book the hotel."),
			want:   "## Task\nPlan Lisbon in May.\n\n## Next steps\nBook the hotel."},
		{name: "later block sees the earlier one", notes: base,
			update: edit("Plan Lisbon.", "Plan Porto.") + "\n" + edit("Plan Porto.", "Plan Porto and Braga."),
			want:   "## Task\nPlan Porto and Braga.\n\n## Next steps\nBook the flight."},
		{name: "empty find appends", notes: base, update: edit("", "## Errors and fixes\nThe TP1350 fare expired."),
			want: base + "\n## Errors and fixes\nThe TP1350 fare expired."},
		{name: "empty replace deletes", notes: base, update: edit("\n\n## Next steps\nBook the flight.", ""), want: "## Task\nPlan Lisbon."},
		{name: "CRLF", notes: base, update: strings.ReplaceAll(edit("Book the flight.", "Book the hotel."), "\n", "\r\n"),
			want: "## Task\nPlan Lisbon.\n\n## Next steps\nBook the hotel."},
		{name: "find missing", notes: base, update: edit("Book the train.", "x"), err: "edit 1: its FIND text is not in the notes"},
		{name: "find twice", notes: "a\na", update: edit("a", "b"), err: "occurs 2 times"},
		{name: "unclosed", notes: base, update: editFind + "\nBook the flight.\n" + editSep + "\nBook the hotel.", err: "edit 1 is not closed"},
		{name: "edits without notes", notes: " ", update: edit("x", "y"), err: "no notes to edit yet"},
		{name: "empty update", notes: base, update: " \n ", err: "the answer is not edits"},
		{name: "edits empty the notes", notes: "only", update: edit("only", ""), err: "leave the notes empty"},
		{name: "no changes", notes: base, update: " NO CHANGES. ", want: base},
		{name: "no changes without notes", notes: "", update: "no changes", err: "no notes yet"},
		{name: "a remark is not notes", notes: base, update: "The notes are current.", err: "the answer is not edits"},
		{name: "a section rewritten whole", notes: base, update: edit("## Next steps\nBook the flight.", "## Next steps\nBook the hotel, then the car."),
			want: "## Task\nPlan Lisbon.\n\n## Next steps\nBook the hotel, then the car."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyNoteEdits(tc.notes, tc.update)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q (got %q)", err, tc.err, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestNotesValidator(t *testing.T) {
	v := notesValidator("## Task\nPlan Lisbon.", 20)
	if fb, ok := v(editFind + "\nPlan Porto.\n" + editSep + "\nx\n" + editReplace); ok || !strings.Contains(fb, "Copy FIND text exactly") {
		t.Fatalf("an edit that does not apply: %q, %v", fb, ok)
	}
	if fb, ok := v(editFind + "\n\n" + editSep + "\n" + strings.Repeat("long notes ", 40) + "\n" + editReplace); ok ||
		!strings.Contains(fb, "over the 20 limit") || !strings.Contains(fb, "edits that replace them with shorter text") {
		t.Fatalf("notes over the limit: %q, %v", fb, ok)
	}
	if fb, ok := v("The notes are current."); ok || !strings.Contains(fb, "or answer "+noteNoChange) {
		t.Fatalf("a remark: %q, %v", fb, ok)
	}
	if fb, ok := v(editFind + "\nPlan Lisbon.\n" + editSep + "\nPlan Porto.\n" + editReplace); !ok {
		t.Fatalf("a good edit was rejected: %q", fb)
	}
}
