package permission

// DecisionOrigin identifies an engine result's source, not an authorization.
// Unknown or unrecognized origins must not be treated as an eligible fallback.
type DecisionOrigin string

const (
	// OriginUnknown carries no certified source information.
	OriginUnknown DecisionOrigin = ""
	// OriginDefaultAsk denotes only the engine's final Default-mode fallback Ask.
	// It does not certify safety, caller intent or the effective execution scope.
	OriginDefaultAsk DecisionOrigin = "default_ask"
)

// Decision is the result of a permission check.
type Decision struct {
	Origin         DecisionOrigin     `json:"origin,omitempty"`
	Behavior       PermissionBehavior `json:"behavior"`
	Message        string             `json:"message"`
	DecisionReason string             `json:"decision_reason,omitempty"`
	UpdatedInput   map[string]any     `json:"updated_input,omitempty"`
	SuggestedRules []Rule             `json:"suggested_rules,omitempty"`

	// BypassImmune marks a safety Ask that cannot be overridden by allow rules
	// in Default/AcceptEdits modes. In Bypass mode this field is ignored; in
	// DontAsk mode the Ask is converted to Deny.
	BypassImmune bool `json:"bypass_immune,omitempty"`
}
