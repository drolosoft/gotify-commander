package stream

import (
	"regexp"
	"strings"
)

// redactedMark replaces every secret removed from a log line.
const redactedMark = "[redacted]"

// tokenQueryValue matches the value of a token= query parameter, which is
// how Gotify accepts a client token in a URL. The value ends at the next
// parameter, whitespace, quote or colon (Go's url.Error ends with ": ").
var tokenQueryValue = regexp.MustCompile(`(token=)[^&\s"':]+`)

// redact removes the configured client token and any token= query value
// from text, so that a log line can be written safely. An empty secret is
// skipped because replacing "" would insert the mark between every byte.
func redact(text, secret string) string {
	if secret != "" {
		text = strings.ReplaceAll(text, secret, redactedMark)
	}

	return tokenQueryValue.ReplaceAllString(text, "${1}"+redactedMark)
}
