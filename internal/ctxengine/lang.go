package ctxengine

import (
	"unicode"
)

func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0x3400 && r <= 0x4DBF: // Extension A
		return true
	case r >= 0xF900 && r <= 0xFAFF: // Compatibility Ideographs
		return true
	}
	return false
}

// CJKRatio returns the share of CJK ideographs among significant
// characters (CJK + letters). Digits, emoji, punctuation and whitespace
// are excluded from both sides so a "你好123😊" counts as pure Chinese.
func CJKRatio(s string) float64 {
	var total, cjk int
	for _, r := range s {
		switch {
		case isCJK(r):
			total++
			cjk++
		case unicode.IsLetter(r):
			total++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(cjk) / float64(total)
}

// IsChinese reports whether the text is predominantly Chinese. Used to
// decide translation direction without an LLM call: outgoing (user's
// own) Chinese goes to the model, anything else is delivered as-is.
func IsChinese(s string) bool {
	return CJKRatio(s) >= 0.5
}

// EstimateTokens gives a rough token count for watermark accounting
// only — a ±20% drift is fine because it only decides when compaction
// fires. Real usage always comes from the LLM response. Heuristics:
// CJK ≈ 1 token/char, Cyrillic ≈ 2 chars/token, Latin ≈ 3 chars/token.
func EstimateTokens(s string) int {
	var cjk, cyr, latin, digits int
	for _, r := range s {
		switch {
		case isCJK(r):
			cjk++
		case r >= 0x0400 && r <= 0x04FF:
			cyr++
		case unicode.IsLetter(r):
			latin++
		case unicode.IsDigit(r):
			digits++
		}
	}
	t := cjk + cyr/2 + latin/3 + digits/2
	if t == 0 {
		t = 1
	}
	return t
}
