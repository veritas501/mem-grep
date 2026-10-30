package util

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	s := Redact("key sk-ant-api03-abcdefghijklmnopqrstuv and token=supersecretvalue and ghp_abcdefghijklmnopqrstuvwxyz12")
	if strings.Contains(s, "abcdefghijklmnop") || strings.Contains(s, "supersecretvalue") {
		t.Fatalf("secret left: %s", s)
	}
	if !strings.Contains(s, "token=[REDACTED]") {
		t.Fatalf("assignment not kept: %s", s)
	}
}

func TestTruncation(t *testing.T) {
	if Truncate("测试数据库", 3) != "测试…" {
		t.Error(Truncate("测试数据库", 3))
	}
	if Tail("abcdef", 3) != "…ef" {
		t.Error(Tail("abcdef", 3))
	}
	if got := TruncateMiddle(strings.Repeat("x", 100), 50); !strings.Contains(got, "chars omitted") {
		t.Error(got)
	}
}

// redactAll is the plain reference: every pattern, no triggers.
func redactAll(text string) string {
	for _, p := range secretPatterns {
		text = p.re.ReplaceAllString(text, p.rep)
	}
	return text
}

func TestRedactTriggersDoNotChangeResults(t *testing.T) {
	samples := []string{
		"plain text with nothing secret, key points: none",
		"PASSWORD: hunter22hunter", "Token=abcdefghijk", "api-key = 'zzzzzzzzzz'", "export ACCESS_KEY=AKIAABCDEFGHIJKLMNOP",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "curl -H 'x: y' secret:shortish",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----",
		"ghp_abcdefghijklmnopqrstuvwxyz12 and github_pat_abcdefghijklmnopqrstuv_x", "xoxb-1234567890-abc AIzaSyA-abcdefghijklmnopqrstuvwxyz12345",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"sk-ant-api03-abcdefghijklmnopqrstuv 测试 pwd=中文密码很长的值", "mixed: Key:VALUE123456 and passwd=abc123def",
	}
	for _, s := range samples {
		if got, want := Redact(s), redactAll(s); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", s, got, want)
		}
	}
}
