package permission

// RuleStore persists permission rules across sessions. Implementations live
// with the settings format they write (see config.NewSettingsRuleStore).
type RuleStore interface {
	// AppendRule durably records one rule. Rules loaded back through the
	// regular settings load path apply to future sessions.
	AppendRule(rule Rule) error
}
