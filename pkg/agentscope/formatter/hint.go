package formatter

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// hintContent keeps nonempty text and media in their original order. Provider
// formatters still decide which media sources and types they support.
func hintContent(hint message.HintBlock) []message.ContentBlock {
	var content []message.ContentBlock
	switch value := hint.Hint.(type) {
	case string:
		if value != "" {
			content = append(content, message.TextBlock{Type: "text", Text: value})
		}
	case []message.ContentBlock:
		for _, block := range value {
			switch b := block.(type) {
			case message.TextBlock:
				if b.Text != "" {
					// Keep the existing text-only hint representation while
					// retaining media between separate text runs.
					if len(content) > 0 {
						if previous, ok := content[len(content)-1].(message.TextBlock); ok {
							previous.Text += "\n" + b.Text
							content[len(content)-1] = previous
							continue
						}
					}
					content = append(content, b)
				}
			case message.DataBlock:
				content = append(content, b)
			}
		}
	}
	return content
}
