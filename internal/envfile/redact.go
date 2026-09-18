package envfile

import "strings"

// Redactor contains resolved dotenv values, including values obtained through
// interpolation. Empty values cannot expose a secret and are ignored.
type Redactor struct {
	values map[string]struct{}
	MaxLen int
}

func (r *Redactor) Add(content string) error {
	entries, err := Parse(content)
	if err != nil {
		return err
	}
	if r.values == nil {
		r.values = make(map[string]struct{})
	}
	for _, entry := range entries {
		_, value, _ := strings.Cut(entry, "=")
		if value == "" {
			continue
		}
		r.values[value] = struct{}{}
		// Docker applies its line tail before returning the stream. A multiline
		// value may arrive without its preceding lines, so hide those fragments too.
		for _, line := range strings.Split(value, "\n") {
			if line != "" {
				r.values[line] = struct{}{}
			}
		}
		if len(value) > r.MaxLen {
			r.MaxLen = len(value)
		}
	}
	return nil
}

// Merge keeps both earlier and current values hidden during concurrent edits.
func (r *Redactor) Merge(other *Redactor) {
	if r.values == nil {
		r.values = make(map[string]struct{})
	}
	for value := range other.values {
		r.values[value] = struct{}{}
	}
	r.MaxLen = max(r.MaxLen, other.MaxLen)
}

func (r *Redactor) Redact(text string) string { return r.Window(text, 0, len(text)) }

// Window redacts matches against the full input before selecting a bounded
// window. Callers read MaxLen extra bytes beyond a truncation boundary so a
// secret crossing it is still recognized. Overlapping matches are merged.
func (r *Redactor) Window(text string, start, end int) string {
	hidden := make([]bool, end-start)
	for value := range r.values {
		marked := start
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], value)
			if i < 0 {
				break
			}
			i += offset
			finish := min(end, i+len(value))
			for j := max(start, i, marked); j < finish; j++ {
				hidden[j-start] = true
			}
			marked = max(marked, finish)
			offset = i + 1
		}
	}
	var out strings.Builder
	for i := start; i < end; i++ {
		if !hidden[i-start] {
			out.WriteByte(text[i])
			continue
		}
		out.WriteString(Placeholder)
		for i+1 < end && hidden[i+1-start] {
			i++
		}
	}
	return out.String()
}
