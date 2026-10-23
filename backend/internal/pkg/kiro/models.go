package kiro

import "regexp"

// Only translate a fully specified Claude version. Never silently substitute
// another model family/version, or treat the catalog's Auto selector as one.
var claudeVersionAlias = regexp.MustCompile(`^claude-(haiku|sonnet|opus|fable)-(\d+)[.-](\d{1,2})(?:-\d{8})?$`)

func NormalizeModelID(model string) string {
	if parts := claudeVersionAlias.FindStringSubmatch(model); parts != nil {
		return "claude-" + parts[1] + "-" + parts[2] + "." + parts[3]
	}
	return model
}
