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

// tokenLimitReader refuses a JSON stream as soon as one string runs past
// maxString raw bytes, or one number or literal runs past maxOther bytes.
// encoding/json builds each whole token before salt can check its length, and
// may quote it in full in an error, so the limit has to apply to the bytes on
// the way in. It also refuses a value nested groupDepth deep, such as one
// entry of the index, that holds more than maxGroupValues values at any
// depth, and a stream whose values at groupDepth or deeper number more than
// maxValues in all. Values are counted by the commas between them:
// encoding/json builds a whole array, such as an entry's parts, before salt
// can count it.
type tokenLimitReader struct {
	r              io.Reader
	maxString      int
	maxOther       int
	groupDepth     int
	maxGroupValues int
	maxValues      int
	inString       bool
	escaped        bool
	n              int // raw bytes in the current string, quotes excluded
	run            int // bytes in the current number or literal
	depth          int // objects and arrays open
	values         int // commas in the current group
	total          int // commas in every group so far
	err            error
}

func newTokenLimitReader(r io.Reader, maxString, maxOther, groupDepth, maxGroupValues, maxValues int) *tokenLimitReader {
	return &tokenLimitReader{r: r, maxString: maxString, maxOther: maxOther, groupDepth: groupDepth, maxGroupValues: maxGroupValues, maxValues: maxValues}
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
				if s.depth < s.groupDepth {
					s.values = 0
				}
				s.run = 0
			case ',':
				if s.depth >= s.groupDepth {
					s.values++
					s.total++
					if s.values >= s.maxGroupValues {
						s.err = fmt.Errorf("more than %d values in one entry", s.maxGroupValues)
						return i, s.err
					}
					if s.total >= s.maxValues {
						s.err = fmt.Errorf("more than %d values in its entries", s.maxValues)
						return i, s.err
					}
				}
				s.run = 0
			case ' ', '\t', '\r', '\n', ':':
				s.run = 0
			default:
				s.run++
				if s.run > s.maxOther {
					s.err = fmt.Errorf("value longer than %d bytes", s.maxOther)
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
		if s.n > s.maxString {
			s.err = fmt.Errorf("string longer than %d bytes", s.maxString)
			return i, s.err
		}
	}
	return n, err
}
