package sources

// Memory files: Markdown with YAML front matter that an agent (ZCode) or mem-grep's own
// distillation (the `mem` adapter, mem.go) keeps beside the sessions. A memory is read like a
// session inside a database: the adapter's ReadRaw builds two JSON lines — a `memory` line with
// what the file says about itself and where it belongs, and a `memory_body` line with its text —
// and openMemory renders them as one turn of ir.RoleMemory lines. The stream holds exactly what is
// rendered, so mg's raw-byte prefilter stays sound.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// memoryRecord is what a memory stream carries.
type memoryRecord struct {
	ID            string  // the session id mg shows
	File          string  // the name printed after MEMORY
	Project       string  // the directory the file was found under (ZCode), informational
	Cwd           string  // the working directory the memory belongs to; "" when unknown
	MtimeMs       float64 // the memory's time
	Name          string
	Description   string
	Type          string // user | feedback | project | reference
	OriginSession string
	Evidence      string // references to the turns a distilled memory came from, space-separated
	Body          string
}

// memoryStream encodes a memory as the two JSON lines openMemory reads.
func memoryStream(m memoryRecord) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"type": "memory", "id": m.ID, "file": m.File, "project": m.Project, "cwd": m.Cwd, "mtime": m.MtimeMs,
		"name": m.Name, "description": m.Description, "memory_type": m.Type, "origin_session": m.OriginSession})
	_ = enc.Encode(map[string]any{"type": "memory_body", "text": m.Body, "evidence": m.Evidence})
	return buf.Bytes()
}

// openMemory renders a memory as a session of one turn: a `MEMORY file [type]: description`
// line, the file's non-empty lines as they are, and an `[evidence: …]` line when the memory names
// the turns it came from.
func openMemory(s *ir.Session, read jsonlRead) *ir.Session {
	var lines []ir.Line
	for _, r := range read.records {
		line := func(text string, turnStart bool) {
			lines = append(lines, ir.Line{EntryID: s.SessionID, TS: s.StartedAt, Role: ir.RoleMemory, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end})
		}
		switch str(r.obj, "type") {
		case "memory":
			s.SessionID = str(r.obj, "id")
			s.Cwd = str(r.obj, "cwd")
			s.Title = str(r.obj, "description")
			if s.Title == "" {
				s.Title = str(r.obj, "name")
			}
			if ms, ok := r.obj["mtime"].(float64); ok && ms > 0 {
				s.StartedAt = time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
			}
			text := "MEMORY " + str(r.obj, "file")
			if t := str(r.obj, "memory_type"); t != "" {
				text += " [" + t + "]"
			}
			if d := str(r.obj, "description"); d != "" {
				text += ": " + util.Truncate(d, 400)
			}
			line(text, true)
		case "memory_body":
			for _, l := range strings.Split(str(r.obj, "text"), "\n") {
				if l = strings.TrimRight(l, " \t\r"); !util.IsSpaceOnly(l) {
					line(util.TruncateMiddle(l, 4000), false)
				}
			}
			if e := str(r.obj, "evidence"); e != "" {
				line("[evidence: "+e+"]", false)
			}
		}
	}
	if s.SessionID == "" {
		return nil
	}
	s.Render = func(from, to int64) []ir.Line {
		var out []ir.Line
		for _, l := range lines {
			if l.Start >= from && (to < 0 || l.Start < to) {
				out = append(out, l)
			}
		}
		return out
	}
	return s
}

var (
	frontMatterKeyRe = regexp.MustCompile(`^\s*([A-Za-z_][\w-]*):\s*(.*)$`)
	blockScalarRe    = regexp.MustCompile(`^[|>][+-]?\d*$`)
)

// memoryFrontMatter splits a memory file into the scalar values of its YAML front matter and its
// body. Nested keys are flattened (metadata.type becomes "type"); this reads the few fields mg
// needs, not YAML.
func memoryFrontMatter(content string) (map[string]string, string) {
	content = strings.ReplaceAll(strings.TrimPrefix(content, "\xef\xbb\xbf"), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if strings.TrimRight(lines[0], " \t") != "---" {
		return nil, content
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, content
	}
	meta := map[string]string{}
	for i := 1; i < end; i++ {
		m := frontMatterKeyRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		if blockScalarRe.MatchString(value) {
			// a block scalar: the more indented lines that follow
			var folded []string
			for i+1 < end && (strings.TrimSpace(lines[i+1]) == "" || strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
				i++
				if t := strings.TrimSpace(lines[i]); t != "" {
					folded = append(folded, t)
				}
			}
			value = strings.Join(folded, " ")
		} else if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		if value != "" {
			meta[key] = value
		}
	}
	return meta, strings.Join(lines[end+1:], "\n")
}
