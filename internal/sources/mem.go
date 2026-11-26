package sources

// mem adapter — the memories mem-grep's own distillation keeps (mgd, docs/mem-distill.md):
// $MG_HOME/memories/<id>.md, one Markdown file per accepted memory:
//
//	---
//	name: test-database
//	description: integration tests run against the real test-db, never a mock
//	type: feedback                 # user | feedback | project | reference
//	project: /home/dev/shop        # the working directory it applies to; empty for every project
//	evidence: [omp:01a181a6-b02b#2, zcode:78b4656a-f8dd#4]
//	created: 2026-11-24T09:00:00Z
//	---
//	body
//
// <id> is eight hex digits and the name, so the 13 characters of a reference tell memories apart.
// A memory is rendered as one turn of `memory` lines, like ZCode's (memory.go), with an
// `[evidence: …]` line naming the turns it was distilled from. Memories still waiting for review
// ($MG_HOME/queue) are not read.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// Mem is the adapter for mem-grep's own memory store.
type Mem struct{}

func (Mem) Kind() ir.SourceKind { return ir.Mem }

func (Mem) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.Mem) {
		for _, f := range walkFiles(filepath.Clean(root), isMarkdown, 0) {
			out = append(out, memFile(f.path, f.mtimeMs))
		}
	}
	return out
}

// Sniff recognises a memory given by path: a Markdown file in a directory called "memories"
// whose front matter names its evidence.
func (Mem) Sniff(path string, head []byte) bool {
	if !isMarkdown(path) || filepath.Base(filepath.Dir(path)) != "memories" {
		return false
	}
	meta, _ := memoryFrontMatter(string(head))
	_, ok := meta["evidence"]
	return ok
}

func memFile(path string, mtimeMs float64) ir.SourceFile {
	return ir.SourceFile{Kind: ir.Mem, Path: path, Key: path, Inner: strings.TrimSuffix(filepath.Base(path), ".md"), MtimeMs: mtimeMs}
}

// MemoryFile is a parsed memory of the store or the review queue.
type MemoryFile struct {
	Name, Description, Type, Project, Created string
	Evidence                                  []string
	Body                                      string
}

// ParseMemoryFile reads a memory in the store's format.
func ParseMemoryFile(content string) MemoryFile {
	meta, body := memoryFrontMatter(content)
	m := MemoryFile{Name: meta["name"], Description: meta["description"], Type: meta["type"], Project: meta["project"], Created: meta["created"], Body: strings.TrimSpace(body)}
	for _, e := range strings.FieldsFunc(strings.Trim(meta["evidence"], "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
		m.Evidence = append(m.Evidence, strings.Trim(e, `"'`))
	}
	return m
}

// ReadRaw builds the memory's stream (memory.go).
func (Mem) ReadRaw(f ir.SourceFile) ([]byte, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	m := ParseMemoryFile(string(data))
	mtime := f.MtimeMs
	if t, ok := util.ParseTime(m.Created); ok {
		mtime = float64(t.UnixNano()) / 1e6 // when it was learned, not when the file was last touched
	} else if st, err := os.Stat(f.Path); err == nil {
		mtime = float64(st.ModTime().UnixNano()) / 1e6
	}
	return memoryStream(memoryRecord{ID: f.Inner, File: filepath.Base(f.Path), Cwd: m.Project, MtimeMs: mtime, Name: m.Name,
		Description: m.Description, Type: m.Type, Evidence: strings.Join(m.Evidence, " "), Body: m.Body}), nil
}

func (a Mem) Open(f ir.SourceFile) (*ir.Session, error) {
	if f.Inner == "" {
		f = memFile(f.Path, f.MtimeMs)
	}
	raw, err := a.ReadRaw(f)
	if err != nil {
		return nil, err
	}
	return a.OpenBytes(f, raw, ScanOptions{})
}

// OpenBytes parses a stream built by ReadRaw. ScanOptions are not used.
func (Mem) OpenBytes(f ir.SourceFile, raw []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(raw)
	return openMemory(&ir.Session{Kind: ir.Mem, File: f, CompleteBytes: read.completeBytes, BadLines: read.badLines}, read), nil
}
