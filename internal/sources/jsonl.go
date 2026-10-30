package sources

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// record is one parsed JSONL line with its byte range, for adapters other than omp (omp keeps
// its own reader because it also handles the title slot and v1 files).
type record struct {
	obj        map[string]any
	line       int // 1-based
	start, end int64
}

type jsonlRead struct {
	records       []record
	completeBytes int64
	size          int64
	badLines      int
}

// parseJSONL parses a JSONL buffer. A final line without "\n" is accepted only if it is complete
// JSON (the agent may still be appending); corrupt lines elsewhere are counted and skipped.
func parseJSONL(buf []byte) jsonlRead {
	r := jsonlRead{size: int64(len(buf))}
	pos, line := 0, 0
	for pos < len(buf) {
		nl := bytes.IndexByte(buf[pos:], '\n')
		last := nl == -1
		end := len(buf)
		if !last {
			end = pos + nl
		}
		start := pos
		text := bytes.TrimSpace(buf[pos:end])
		pos = end + 1
		line++
		if len(text) == 0 {
			if !last {
				r.completeBytes = int64(pos)
			}
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(text, &obj); err != nil {
			if !last {
				r.badLines++
				r.completeBytes = int64(pos)
			}
			continue
		}
		recEnd := int64(pos)
		if last {
			recEnd = int64(len(buf))
		}
		r.records = append(r.records, record{obj: obj, line: line, start: int64(start), end: recEnd})
		r.completeBytes = recEnd
	}
	return r
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

func isZstd(head []byte) bool { return bytes.HasPrefix(head, zstdMagic) }

// readMaybeCompressed returns the file's bytes, decompressing zstd (Codex .jsonl.zst rollouts).
// Offsets always refer to the decompressed bytes.
func readMaybeCompressed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil || !isZstd(data) {
		return data, err
	}
	return decompress(data)
}

func decompress(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(data, nil)
}

type fileInfo struct {
	path    string
	size    int64
	mtimeMs float64
}

// walkFiles lists files under root (up to maxDepth directories deep) whose name passes accept.
func walkFiles(root string, accept func(name string) bool, maxDepth int) []fileInfo {
	var out []fileInfo
	base := strings.Count(filepath.Clean(root), string(filepath.Separator))
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.Count(filepath.Clean(p), string(filepath.Separator))-base > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !accept(d.Name()) {
			return nil
		}
		if st, err := d.Info(); err == nil {
			out = append(out, fileInfo{p, st.Size(), float64(st.ModTime().UnixNano()) / 1e6})
		}
		return nil
	})
	return out
}

// Small accessors for map[string]any records.

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func obj(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case nil:
		return false
	case string:
		return x != ""
	case float64:
		return x != 0
	}
	return true
}
