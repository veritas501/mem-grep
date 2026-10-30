// Package util holds the small text helpers shared by every other package: secret redaction and
// truncation.
//
// Lengths are counted in runes, not bytes, so limits mean the same for Chinese and English text.
package util

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Truncate cuts s to at most max runes, ending with "…" when cut.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s // fewer bytes than max, so fewer runes
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max < 1 {
		max = 1
	}
	return string(r[:max-1]) + "…"
}

// TruncateMiddle keeps head and tail; useful for logs and long messages.
func TruncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := int(math.Floor(float64(max) * 0.6))
	tail := max - head - 20
	if tail < 0 {
		tail = 0
	}
	return fmt.Sprintf("%s\n…[%d chars omitted]…\n%s", string(r[:head]), len(r)-head-tail, string(r[len(r)-tail:]))
}

// Tail keeps the last max runes, prefixed with "…" when cut.
func Tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max+1:])
}

// RuneLen is the length of s in runes.

// ---------------------------------------------------------------------------
// Secret redaction: applied before anything is sent to an LLM or stored.
// ---------------------------------------------------------------------------

// Each pattern has a cheap trigger: the regexp runs only when the text contains one of the
// substrings any match must contain (case-insensitively for the assignment pattern). Most lines
// contain none, which keeps redaction from dominating the cost of rendering transcripts.
var secretPatterns = []struct {
	re       *regexp.Regexp
	rep      string
	triggers []string
	fold     bool
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]", []string{"PRIVATE KEY-----"}, false},
	{regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{16,}`), "[REDACTED]", []string{"sk-"}, false},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), "[REDACTED]", []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, false},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), "[REDACTED]", []string{"github_pat_"}, false},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED]", []string{"AKIA"}, false},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), "[REDACTED]", []string{"xox"}, false},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`), "[REDACTED]", []string{"AIza"}, false},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), "[REDACTED JWT]", []string{"eyJ"}, false},
	{regexp.MustCompile(`\b(Bearer)\s+[A-Za-z0-9._~+/-]{16,}=*`), "${1} [REDACTED]", []string{"Bearer"}, false},
	{regexp.MustCompile(`(?i)\b((?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)["']?\s*[=:]\s*["']?)[^\s"'` + "`" + `,;]{6,}`), "${1}[REDACTED]",
		[]string{"pass", "pwd", "secret", "token", "key"}, true},
}

// Redact replaces API keys, tokens, private keys, JWTs and password assignments.
func Redact(text string) string {
	var lower string
	for _, p := range secretPatterns {
		hay := text
		if p.fold {
			if !strings.ContainsAny(text, "=:") {
				continue
			}
			if lower == "" {
				lower = strings.ToLower(text)
			}
			hay = lower
		}
		hit := false
		for _, t := range p.triggers {
			if strings.Contains(hay, t) {
				hit = true
				break
			}
		}
		if hit {
			text = p.re.ReplaceAllString(text, p.rep)
			lower = ""
		}
	}
	return text
}

// ---------------------------------------------------------------------------

// ShortPath abbreviates the home directory to "~"; empty (a session with no recorded directory) prints as "(global)".
func ShortPath(p string) string {
	if p == "" {
		return "(global)"
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// ParseTime parses the ISO timestamps found in sessions and the database.
func ParseTime(iso string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02"} {
		if t, err := time.Parse(layout, iso); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// IsSpaceOnly reports whether s has no visible characters.
func IsSpaceOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) }) == -1
}
