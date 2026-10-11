package seal

import (
	"fmt"
	"io"
)

// Limits on single JSON tokens in the index. json.Marshal can write a byte as
// a 6-byte escape such as <, so a string of maxIndexString bytes can take
// up to six times as many. Numbers and true, false and null need far less than
// maxIndexToken: an int64 is at most 20 characters.
const (
	maxIndexJSONString = 6 * maxIndexString
	maxIndexToken      = 64
)

// tokenLimits are the limits a tokenLimitReader applies to the index.
type tokenLimits struct {
	String int // raw bytes in one string
	Other  int // bytes in one number or literal
	Depth  int // how deeply the values counted sit, such as in one entry
	Values int // values at Depth or deeper, in all
}

// tokenLimitReader refuses a JSON stream as soon as one string runs past
// String raw bytes, or one number or literal runs past Other bytes.
// encoding/json builds each whole token before salt can check its length, and
// may quote it in full in an error, so the limit has to apply to the bytes on
// the way in. It also refuses the stream once the values Depth deep or deeper,
// those in the index's entries, number more than Values in all. They are
// counted by the commas between them: encoding/json builds a whole array, such
// as an entry's parts, before salt can count it.
type tokenLimitReader struct {
	r        io.Reader
	lim      tokenLimits
	inString bool
	escaped  bool
	n        int // raw bytes in the current string, quotes excluded
	run      int // bytes in the current number or literal
	depth    int // objects and arrays open
	values   int // commas Depth deep or deeper so far
	err      error
}

func newTokenLimitReader(r io.Reader, lim tokenLimits) *tokenLimitReader {
	return &tokenLimitReader{r: r, lim: lim}
}

func (s *tokenLimitReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	for i, c := range p[:n] {
		if !s.inString {
			switch c {
			case '"':
				s.inString, s.n, s.run = true, 0, 0
			case '{', '[':
				s.depth++
				s.run = 0
			case '}', ']':
				s.depth--
				s.run = 0
			case ',':
				if s.depth >= s.lim.Depth {
					s.values++
					if s.values >= s.lim.Values {
						s.err = fmt.Errorf("more than %d values in its entries", s.lim.Values)
						return i, s.err
					}
				}
				s.run = 0
			case ' ', '\t', '\r', '\n', ':':
				s.run = 0
			default:
				s.run++
				if s.run > s.lim.Other {
					s.err = fmt.Errorf("value longer than %d bytes", s.lim.Other)
					return i, s.err
				}
			}
			continue
		}
		switch {
		case s.escaped:
			s.escaped = false
		case c == '\\':
			s.escaped = true
		case c == '"':
			s.inString = false
			continue
		}
		s.n++
		if s.n > s.lim.String {
			s.err = fmt.Errorf("string longer than %d bytes", s.lim.String)
			return i, s.err
		}
	}
	return n, err
}
