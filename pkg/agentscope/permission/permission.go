// Package permission implements a permission system for tool usage control.
//
// The system supports five permission modes (Default, AcceptEdits, Explore,
// Bypass, DontAsk) and evaluates tool execution requests against configurable
// rules. Each tool can implement fine-grained permission checking via the
// Checker interface.
package permission
