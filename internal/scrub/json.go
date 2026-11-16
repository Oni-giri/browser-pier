package scrub

import (
	"bytes"
	"encoding/json"
)

// JSON scrubs a JSON document's strings, keys included: each is decoded,
// scrubbed as text and, if that changed it, written back the way
// encoding/json writes strings. Everything else stays byte for byte, so
// a secret holding a quote can never break the document's structure.
// A body that isn't valid JSON is scrubbed as text. Numbers aren't
// touched: a secret is text.
func (s *Scrubber) JSON(body []byte) []byte {
	if s == nil || len(body) == 0 {
		return body
	}
	if !json.Valid(body) {
		text := string(body)
		if out := s.String(text); out != text {
			return []byte(out)
		}
		return body
	}
	var out []byte
	last := 0
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		// In valid JSON, a quote outside a string opens one; find its end.
		j := i + 1
		for body[j] != '"' {
			if body[j] == '\\' {
				j++
			}
			j++
		}
		var str string
		if json.Unmarshal(body[i:j+1], &str) == nil {
			if clean := s.String(str); clean != str {
				out = append(out, body[last:i]...)
				out = append(out, quote(clean)...)
				last = j + 1
			}
		}
		i = j
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}

// quote is s as a JSON string, as encoding/json writes it.
func quote(s string) []byte {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(s) // a string always encodes
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// Union is a Scrubber for both a's and b's secrets.
func Union(a, b *Scrubber) *Scrubber {
	switch {
	case a == nil || a == b:
		return b
	case b == nil:
		return a
	}
	return New(append(append([]string(nil), a.secrets...), b.secrets...))
}
