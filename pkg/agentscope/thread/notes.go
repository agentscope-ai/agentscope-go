package thread

import (
	"errors"
	"fmt"
	"strings"
)

// Edits of a participant's own notes (SummaryPolicy.OwnUpdatePrompt). An
// update answers with FIND/REPLACE blocks against the previous notes, the
// way Claude Code's session memory may only edit its notes file, so an
// update costs the output of its edits and can never replace the notes
// with a remark about them. Each marker stands on a line of its own.
const (
	editFind    = "<<<<<<< FIND"
	editSep     = "======="
	editReplace = ">>>>>>> REPLACE"
	// noteNoChange is an update that leaves the notes as they are, the way
	// a Claude Code memory fork that makes no edit leaves its file.
	noteNoChange = "NO CHANGES"
)

// noteEdit is one FIND/REPLACE block.
type noteEdit struct{ find, repl string }

// applyNoteEdits applies an update to notes. Without notes the update is
// the complete first notes; with notes it is edits, or NO CHANGES, which
// keeps them. Each FIND text must occur exactly once in the notes as the
// earlier blocks left them; an empty one appends its replacement. The
// error says which block failed and why, for the model.
func applyNoteEdits(notes, update string) (string, error) {
	update = strings.ReplaceAll(update, "\r\n", "\n")
	if strings.EqualFold(strings.Trim(strings.TrimSpace(update), "."), noteNoChange) {
		if strings.TrimSpace(notes) == "" {
			return "", errors.New("there are no notes yet; answer with the complete notes")
		}
		return notes, nil
	}
	edits, err := parseNoteEdits(update)
	if err != nil {
		return "", err
	}
	hasNotes := strings.TrimSpace(notes) != ""
	switch {
	case edits == nil && hasNotes:
		return "", fmt.Errorf("the answer is not edits; answer with edits, or %s if nothing needs to change", noteNoChange)
	case edits == nil:
		out := strings.TrimSpace(update)
		if out == "" {
			return "", errors.New("the update is empty")
		}
		return out, nil
	case !hasNotes:
		return "", errors.New("there are no notes to edit yet; answer with the complete notes")
	}
	out := notes
	for i, e := range edits {
		if strings.TrimSpace(e.find) == "" {
			if strings.TrimSpace(e.repl) != "" {
				out = strings.TrimRight(out, "\n") + "\n" + e.repl
			}
			continue
		}
		switch n := strings.Count(out, e.find); {
		case n == 0:
			return "", fmt.Errorf("edit %d: its FIND text is not in the notes", i+1)
		case n > 1:
			return "", fmt.Errorf("edit %d: its FIND text occurs %d times in the notes; include more of the text around it", i+1, n)
		}
		out = strings.Replace(out, e.find, e.repl, 1)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("the edits leave the notes empty")
	}
	return out, nil
}

// parseNoteEdits splits update into its blocks; nil when it has no FIND
// line, so it is the complete notes.
func parseNoteEdits(update string) ([]noteEdit, error) {
	lines := strings.Split(update, "\n")
	var edits []noteEdit
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != editFind {
			continue
		}
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != editSep {
			j++
		}
		k := j + 1
		for k < len(lines) && strings.TrimSpace(lines[k]) != editReplace {
			k++
		}
		if k >= len(lines) {
			return nil, fmt.Errorf("edit %d is not closed: after its FIND text it needs a %q line, then the replacement, then a %q line",
				len(edits)+1, editSep, editReplace)
		}
		edits = append(edits, noteEdit{find: strings.Join(lines[i+1:j], "\n"), repl: strings.Join(lines[j+1:k], "\n")})
		i = k
	}
	return edits, nil
}

// notesValidator accepts an update that applies to base and leaves notes of
// at most limit estimated tokens.
func notesValidator(base string, limit int) func(string) (string, bool) {
	return func(text string) (string, bool) {
		notes, err := applyNoteEdits(base, text)
		if err != nil {
			return err.Error() + ". Copy FIND text exactly from the notes, or answer " + noteNoChange + ".", false
		}
		if n := estimateText(notes); n > limit {
			return fmt.Sprintf("The notes would be %d tokens, over the %d limit. Condense older details: "+
				"answer with edits that replace them with shorter text.", n, limit), false
		}
		return "", true
	}
}
