package tool

import (
	"testing"
)

func TestToolkitOwnsRegistrationSlice(t *testing.T) {
	for _, group := range []bool{false, true} {
		first, second := ReadTool(), GrepTool()
		input := make([]Tool, 1, 2)
		input[0] = first
		tk := NewToolkit()
		if group {
			tk.AddGroup("files", input...)
		} else {
			tk = NewToolkit(input...)
		}
		input[0] = second
		input = append(input, second)
		if len(input) != 2 {
			t.Fatal("append failed")
		}
		schemas := tk.GetToolSchemas()
		if len(schemas) != 1 || schemas[0].Function.Name != first.Name() {
			t.Fatalf("caller changed registered tools: %+v", schemas)
		}
	}
}
