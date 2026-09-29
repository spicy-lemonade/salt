// Package keys creates, derives, wraps and stores salt's age identities.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// PhraseWords is the length of a recovery phrase: 128 bits of entropy plus a
// 4-bit checksum, 11 bits per word.
const PhraseWords = 12

const entropyBytes = 16

//go:embed bip39_english.txt
var wordlistText string

var (
	wordlist  = strings.Fields(wordlistText)
	wordIndex = func() map[string]int {
		m := make(map[string]int, len(wordlist))
		for i, w := range wordlist {
			m[w] = i
		}
		return m
	}()
)

// NewEntropy returns fresh random entropy for a recovery phrase.
func NewEntropy() ([]byte, error) {
	e := make([]byte, entropyBytes)
	if _, err := rand.Read(e); err != nil {
		return nil, err
	}
	return e, nil
}

// EncodePhrase turns 16 bytes of entropy into 12 BIP39 words.
func EncodePhrase(entropy []byte) ([]string, error) {
	if len(entropy) != entropyBytes {
		return nil, fmt.Errorf("entropy must be %d bytes", entropyBytes)
	}
	// 128 entropy bits followed by the top 4 bits of SHA-256(entropy).
	bits := append(append([]byte{}, entropy...), sha256.Sum256(entropy)[0])
	words := make([]string, PhraseWords)
	for i := range words {
		words[i] = wordlist[readBits(bits, i*11, 11)]
	}
	return words, nil
}

// DecodePhrase parses a recovery phrase back into entropy. Words are matched
// case-insensitively, either in full or by their first four letters (unique
// in the BIP39 list). The checksum catches most typos and swapped words.
func DecodePhrase(words []string) ([]byte, error) {
	if len(words) != PhraseWords {
		return nil, fmt.Errorf("a recovery phrase has %d words, got %d", PhraseWords, len(words))
	}
	bits := make([]byte, entropyBytes+1)
	for i, w := range words {
		idx, err := LookupWord(w)
		if err != nil {
			return nil, fmt.Errorf("word %d: %w", i+1, err)
		}
		writeBits(bits, i*11, 11, idx)
	}
	entropy := bits[:entropyBytes]
	if bits[entropyBytes]>>4 != sha256.Sum256(entropy)[0]>>4 {
		return nil, ErrChecksum
	}
	return entropy, nil
}

// ErrChecksum means every word is valid but the phrase as a whole is not.
var ErrChecksum = errors.New("these words are not a valid recovery phrase (checksum mismatch); check for a typo or swapped words")

// LookupWord returns the index of a BIP39 word, accepting a unique 4-letter
// prefix.
func LookupWord(w string) (int, error) {
	w = strings.ToLower(strings.TrimSpace(w))
	if i, ok := wordIndex[w]; ok {
		return i, nil
	}
	if len(w) >= 4 {
		for i, cand := range wordlist {
			if strings.HasPrefix(cand, w) {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("%q is not a recovery phrase word", w)
}

// SplitPhrase splits user input on spaces, commas and newlines.
func SplitPhrase(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\n' || r == '\t' || r == '\r'
	})
}

func readBits(b []byte, off, n int) int {
	v := 0
	for i := 0; i < n; i++ {
		bit := (b[(off+i)/8] >> (7 - uint((off+i)%8))) & 1
		v = v<<1 | int(bit)
	}
	return v
}

func writeBits(b []byte, off, n, v int) {
	for i := 0; i < n; i++ {
		if (v>>(n-1-i))&1 == 1 {
			b[(off+i)/8] |= 1 << (7 - uint((off+i)%8))
		}
	}
}
