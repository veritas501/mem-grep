// Command mg (mem-grep) searches past coding-agent sessions like rg: no index, no model, no network.
package main

import (
	"os"
	"runtime/debug"

	"github.com/veritas501/mem-grep/internal/cli/mg"
)

func main() {
	// A search decodes a lot of short-lived JSON. Collect garbage less often (about half the
	// time of a full scan otherwise goes to the collector), with a soft cap so memory stays
	// bounded. GOGC / GOMEMLIMIT in the environment still win.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(1 << 30)
	}
	os.Exit(mg.Execute(os.Args[1:]))
}
