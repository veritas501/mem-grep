package version

import "testing"

func TestDefaultsAreRecognisable(t *testing.T) {
	// `go test` builds without the Makefile's -ldflags, so the defaults must show.
	if got := String("mem-grep"); got != "mem-grep dev (commit=none, built=unknown)" {
		t.Fatal(got)
	}
}
