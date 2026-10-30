package grep

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HitRef is the reference of a hit: "<agent>:<id prefix>#<turn>".
func (h Hit) Ref() string { return h.Session.Ref() + "#" + strconv.Itoa(h.Turn.Index) }

// Snippet cuts text to about width runes around its first match and shifts the match ranges to
// the cut text (ranges outside it are dropped). Newlines become " ⏎ " so a line stays one line.
// width <= 0 keeps the whole text.
func Snippet(text string, ranges [][2]int, width int) (string, [][2]int) {
	text, ranges = flatten(text, ranges)
	if width <= 0 || utf8.RuneCountInString(text) <= width {
		return text, ranges
	}
	// byte offset of each rune, plus the end
	offs := make([]int, 0, len(text)+1)
	for i := range text {
		offs = append(offs, i)
	}
	offs = append(offs, len(text))
	runeAt := func(b int) int { return sort.SearchInts(offs, b) }
	n := len(offs) - 1
	startR := 0
	if len(ranges) > 0 {
		startR = runeAt(ranges[0][0]) - width/3
	}
	startR = max(0, min(startR, n-width))
	endR := min(n, startR+width)
	from, to := offs[startR], offs[endR]
	prefix, suffix := "", ""
	if startR > 0 {
		prefix = "…"
	}
	if endR < n {
		suffix = "…"
	}
	var out [][2]int
	for _, r := range ranges {
		if r[0] >= from && r[1] <= to {
			out = append(out, [2]int{r[0] - from + len(prefix), r[1] - from + len(prefix)})
		}
	}
	return prefix + text[from:to] + suffix, out
}

// flatten replaces newlines (with surrounding indentation) by " ⏎ ", keeping ranges aligned.
func flatten(text string, ranges [][2]int) (string, [][2]int) {
	if !strings.Contains(text, "\n") {
		return text, ranges
	}
	var b strings.Builder
	shift := make([]int, len(text)+1) // new offset of each old byte offset
	i := 0
	for i < len(text) {
		if text[i] == '\n' {
			k := i + 1
			for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
				k++
			}
			for x := i; x < k; x++ {
				shift[x] = b.Len()
			}
			b.WriteString(" ⏎ ")
			i = k
			continue
		}
		shift[i] = b.Len()
		b.WriteByte(text[i])
		i++
	}
	shift[len(text)] = b.Len()
	out := make([][2]int, len(ranges))
	for k, r := range ranges {
		out[k] = [2]int{shift[r[0]], shift[r[1]]}
	}
	return b.String(), out
}

// TopEntry counts one distinct matched string.
type TopEntry struct {
	Match    string `json:"match"`
	Count    int    `json:"count"`
	Sessions int    `json:"sessions"`
	Last     string `json:"last"`
	Example  string `json:"example"` // the newest hit's ref
}

// Top counts the distinct strings the patterns matched across hits (case-folded when fold),
// most frequent first. It answers "which X did we use most" questions in one call.
func Top(hits []Hit, fold bool) []TopEntry {
	type acc struct {
		e        TopEntry
		sessions map[string]bool
	}
	by := map[string]*acc{}
	for _, h := range hits { // hits are newest first
		for li, rs := range h.Matched {
			text := h.Turn.Lines[li].Text
			for _, r := range rs {
				m := text[r[0]:r[1]]
				key := m
				if fold {
					key = strings.ToLower(m)
				}
				a := by[key]
				if a == nil {
					a = &acc{e: TopEntry{Match: m, Last: h.Turn.TS, Example: h.Ref()}, sessions: map[string]bool{}}
					by[key] = a
				}
				a.e.Count++
				a.sessions[h.Session.Key] = true
			}
		}
	}
	out := make([]TopEntry, 0, len(by))
	for _, a := range by {
		a.e.Sessions = len(a.sessions)
		out = append(out, a.e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Match < out[j].Match
	})
	return out
}
