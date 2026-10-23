package store

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"strings"
)

// Room codes avoid 1 and 7: over a noisy cab / radio "一 yī" and "七 qī"
// are the most commonly confused digits.
const codeDigits = "02345689"

func randomCode(length int) string {
	var b strings.Builder
	for i := range length {
		alphabet := codeDigits
		if i == 0 {
			alphabet = codeDigits[1:] // no leading zero
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

func randomID(prefix string) string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return prefix + hex.EncodeToString(buf)
}

// spokenDigits maps Chinese numerals and radio-operator slang to digits.
var spokenDigits = map[rune]byte{
	'零': '0', '〇': '0', '洞': '0',
	'一': '1', '幺': '1', '壹': '1',
	'二': '2', '两': '2', '贰': '2',
	'三': '3', '叁': '3',
	'四': '4', '肆': '4',
	'五': '5', '伍': '5',
	'六': '6', '陆': '6',
	'七': '7', '拐': '7', '柒': '7',
	'八': '8', '捌': '8',
	'九': '9', '勾': '9', '玖': '9',
}

// ParseCode extracts a room code from free text, e.g. "房间三七二幺" → "3721".
// It returns "" when the text contains fewer than 4 digits.
func ParseCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '０' && r <= '９':
			b.WriteByte(byte('0' + r - '０'))
		default:
			if d, ok := spokenDigits[r]; ok {
				b.WriteByte(d)
			}
		}
	}
	if b.Len() < 4 {
		return ""
	}
	return b.String()
}
