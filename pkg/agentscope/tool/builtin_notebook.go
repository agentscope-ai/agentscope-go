package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var notebookEditSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"notebook_path": {
			"type": "string",
			"description": "The absolute path to the .ipynb notebook file"
		},
		"cell_id": {
			"type": "string",
			"description": "The ID of the cell to edit. When inserting, the new cell is placed after this cell."
		},
		"new_source": {
			"type": "string",
			"description": "The new source content for the cell"
		},
		"cell_type": {
			"type": "string",
			"enum": ["code", "markdown"],
			"description": "The cell type (required for insert mode)"
		},
		"edit_mode": {
			"type": "string",
			"enum": ["replace", "insert", "delete"],
			"description": "The edit mode: replace (default), insert, or delete"
		}
	},
	"required": ["notebook_path", "new_source"]
}`)

type notebookEditTool struct {
	BaseTool
}

type notebook struct {
	Cells         []notebookCell `json:"cells"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	NbformatMajor int            `json:"nbformat"`
	NbformatMinor int            `json:"nbformat_minor"`
}

type notebookCell struct {
	ID        string         `json:"id,omitempty"`
	CellType  string         `json:"cell_type"`
	Source    any            `json:"source"` // string or []string
	Metadata  map[string]any `json:"metadata,omitempty"`
	Outputs   []any          `json:"outputs,omitempty"`
	ExecCount *int           `json:"execution_count,omitempty"`
}

func (c *notebookCell) getSource() string {
	switch v := c.Source.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func (c *notebookCell) setSource(s string) {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	result := make([]any, len(lines))
	for i, line := range lines {
		result[i] = line
	}
	c.Source = result
}

func (t *notebookEditTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	nbPath, _ := args["notebook_path"].(string)
	if nbPath == "" {
		return NewErrorResponse(fmt.Errorf("notebook_path is required")), nil
	}
	if filepath.Ext(nbPath) != ".ipynb" {
		return NewErrorResponse(fmt.Errorf("file must be a .ipynb notebook")), nil
	}

	b := GetBackend(ctx)

	data, err := b.ReadFile(ctx, nbPath)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("read notebook: %w", err)), nil
	}

	var nb notebook
	if err := json.Unmarshal(data, &nb); err != nil {
		return NewErrorResponse(fmt.Errorf("parse notebook: %w", err)), nil
	}

	mode, _ := args["edit_mode"].(string)
	if mode == "" {
		mode = "replace"
	}

	cellID, _ := args["cell_id"].(string)
	newSource, _ := args["new_source"].(string)
	cellType, _ := args["cell_type"].(string)

	var resultMsg string

	switch mode {
	case "replace":
		if cellID == "" {
			return NewErrorResponse(fmt.Errorf("cell_id is required for replace mode")), nil
		}
		idx := findCellIndex(nb.Cells, cellID)
		if idx < 0 {
			return NewErrorResponse(fmt.Errorf("cell %q not found", cellID)), nil
		}
		nb.Cells[idx].setSource(newSource)
		if cellType != "" {
			nb.Cells[idx].CellType = cellType
		}
		nb.Cells[idx].Outputs = nil
		nb.Cells[idx].ExecCount = nil
		resultMsg = fmt.Sprintf("Replaced cell %q", cellID)

	case "insert":
		if cellType == "" {
			return NewErrorResponse(fmt.Errorf("cell_type is required for insert mode")), nil
		}
		newCell := notebookCell{
			ID:       fmt.Sprintf("cell_%d", len(nb.Cells)+1),
			CellType: cellType,
			Metadata: map[string]any{},
		}
		newCell.setSource(newSource)
		if cellType == "code" {
			newCell.Outputs = []any{}
		}

		if cellID == "" {
			nb.Cells = append([]notebookCell{newCell}, nb.Cells...)
		} else {
			idx := findCellIndex(nb.Cells, cellID)
			if idx < 0 {
				return NewErrorResponse(fmt.Errorf("cell %q not found", cellID)), nil
			}
			nb.Cells = append(nb.Cells[:idx+1], append([]notebookCell{newCell}, nb.Cells[idx+1:]...)...)
		}
		resultMsg = fmt.Sprintf("Inserted %s cell after %q", cellType, cellID)

	case "delete":
		if cellID == "" {
			return NewErrorResponse(fmt.Errorf("cell_id is required for delete mode")), nil
		}
		idx := findCellIndex(nb.Cells, cellID)
		if idx < 0 {
			return NewErrorResponse(fmt.Errorf("cell %q not found", cellID)), nil
		}
		nb.Cells = append(nb.Cells[:idx], nb.Cells[idx+1:]...)
		resultMsg = fmt.Sprintf("Deleted cell %q", cellID)

	default:
		return NewErrorResponse(fmt.Errorf("invalid edit_mode %q", mode)), nil
	}

	output, err := json.MarshalIndent(nb, "", " ")
	if err != nil {
		return NewErrorResponse(fmt.Errorf("marshal notebook: %w", err)), nil
	}
	output = append(output, '\n')

	if err := b.WriteFile(ctx, nbPath, output); err != nil {
		return NewErrorResponse(fmt.Errorf("write notebook: %w", err)), nil
	}

	return NewTextResponse(resultMsg), nil
}

func findCellIndex(cells []notebookCell, id string) int {
	for i, c := range cells {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// NotebookEditTool returns a tool for editing Jupyter .ipynb notebook files.
func NotebookEditTool() Tool {
	return &notebookEditTool{
		BaseTool: BaseTool{
			ToolName:        "NotebookEdit",
			ToolDescription: "Replace, insert, or delete a cell in a Jupyter notebook (.ipynb file).",
			ToolSchema:      notebookEditSchema,
		},
	}
}

// ReadNotebook returns a formatted string representation of a notebook file.
func ReadNotebook(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var nb notebook
	if err := json.Unmarshal(data, &nb); err != nil {
		return "", fmt.Errorf("parse notebook: %w", err)
	}

	var b strings.Builder
	for _, cell := range nb.Cells {
		id := cell.ID
		if id == "" {
			id = "unknown"
		}
		fmt.Fprintf(&b, "<cell id=%q type=%q>\n", id, cell.CellType)
		b.WriteString(cell.getSource())
		if !strings.HasSuffix(cell.getSource(), "\n") {
			b.WriteByte('\n')
		}

		if cell.CellType == "code" && len(cell.Outputs) > 0 {
			b.WriteString("[output]\n")
			for _, out := range cell.Outputs {
				if m, ok := out.(map[string]any); ok {
					if text, ok := m["text"]; ok {
						switch v := text.(type) {
						case string:
							b.WriteString(v)
						case []any:
							for _, line := range v {
								if s, ok := line.(string); ok {
									b.WriteString(s)
								}
							}
						}
					}
				}
			}
		}
		b.WriteString("</cell>\n\n")
	}

	return b.String(), nil
}

// compile-time interface check
var _ Tool = (*notebookEditTool)(nil)
