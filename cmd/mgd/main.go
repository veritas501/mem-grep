// Command mgd (mem-grep distill) keeps the state for turning past sessions into memories: which
// turns have been read, a review queue and the store mg searches. It holds no model: an agent
// reads the pending turns through it and hands memories back (skill/mem-distill).
package main

import (
	"os"

	"github.com/veritas501/mem-grep/internal/cli/mgd"
)

func main() { os.Exit(mgd.Execute(os.Args[1:])) }
