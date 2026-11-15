package hub

import "context"

// CardKind identifies the type of hub card.
type CardKind string

const (
	CardKindMCP   CardKind = "mcp"
	CardKindSkill CardKind = "skill"
)

// Card represents an installable item in the hub.
type Card struct {
	ID          string            `json:"id"`
	Owner       string            `json:"owner"`
	Kind        CardKind          `json:"kind"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Version     string            `json:"version"`
	IconURL     string            `json:"icon_url,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Config      map[string]any    `json:"config,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

// ListOptions controls pagination and filtering for hub listings.
type ListOptions struct {
	Query  string   // search query
	Tags   []string // filter by tags
	Kind   CardKind // filter by kind
	Owner  string   // filter by owner
	Cursor string   // pagination cursor
	Limit  int      // page size (default 20, max 100)
}

// ListResult is a paginated list of cards.
type ListResult struct {
	Cards      []Card `json:"cards"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
	Total      int    `json:"total,omitempty"`
}

// Hub is the interface for a remote registry of installable components.
type Hub interface {
	// ID returns the unique identifier for this hub.
	ID() string
	// DisplayName returns the user-facing name.
	DisplayName() string
	// List returns cards matching the given options.
	List(ctx context.Context, opts *ListOptions) (*ListResult, error)
	// Get retrieves a specific card by ID.
	Get(ctx context.Context, cardID string) (*Card, error)
	// Install downloads and installs a card's resources.
	Install(ctx context.Context, cardID string, targetDir string) error
	// Close releases any resources held by the hub.
	Close() error
}
