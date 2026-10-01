package transcript

import "regexp"

var parserMinor = regexp.MustCompile(`@([0-9]+)\.[0-9]+`)

// ReparseKey retains major versions and effective policy changes, while
// excluding minor parser releases. It also handles composite Codex versions.
func ReparseKey(version string) string {
	return parserMinor.ReplaceAllString(version, "@${1}")
}
