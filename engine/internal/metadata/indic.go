package metadata

import (
	"strings"
	"unicode"
)

// DetectScript identifies the primary Indic or Latin script of a string.
func DetectScript(s string) string {
	for _, r := range s {
		switch {
		case r >= 0x0900 && r <= 0x097F:
			return "devanagari"
		case r >= 0x0A00 && r <= 0x0A7F:
			return "gurmukhi"
		case r >= 0x0B80 && r <= 0x0BFF:
			return "tamil"
		case r >= 0x0C00 && r <= 0x0C7F:
			return "telugu"
		}
	}
	return "latin"
}

var devanagariVowels = map[rune]string{
	'अ': "a", 'आ': "aa", 'इ': "i", 'ई': "ee", 'उ': "u", 'ऊ': "oo",
	'ऋ': "ri", 'ए': "e", 'ऐ': "ai", 'ओ': "o", 'औ': "au",
}

var devanagariConsonants = map[rune]string{
	'क': "k", 'ख': "kh", 'ग': "g", 'घ': "gh", 'ङ': "ng",
	'च': "ch", 'छ': "chh", 'ज': "j", 'झ': "jh", 'ञ': "ny",
	'ट': "t", 'ठ': "th", 'ड': "d", 'ढ': "dh", 'ण': "n",
	'त': "t", 'थ': "th", 'द': "d", 'ध': "dh", 'न': "n",
	'प': "p", 'फ': "ph", 'ब': "b", 'भ': "bh", 'म': "m",
	'य': "y", 'र': "r", 'ल': "l", 'व': "v",
	'श': "sh", 'ष': "sh", 'स': "s", 'ह': "h",
	0x0958: "q", 0x0959: "kh", 0x095A: "gh", 0x095B: "z", 0x095C: "r", 0x095D: "rh", 0x095E: "f",
}

var devanagariMatras = map[rune]string{
	'ा': "aa", 'ि': "i", 'ी': "ee", 'ु': "u", 'ू': "oo",
	'ृ': "ri", 'े': "e", 'ै': "ai", 'ो': "o", 'ौ': "au",
}

var gurmukhiVowels = map[rune]string{
	'ਅ': "a", 'ਆ': "aa", 'ਇ': "i", 'ਈ': "ee", 'ਉ': "u", 'ਊ': "oo",
	'ਏ': "e", 'ਐ': "ai", 'ਓ': "o", 'ਔ': "au",
}

var gurmukhiConsonants = map[rune]string{
	'ਕ': "k", 'ਖ': "kh", 'ਗ': "g", 'ਘ': "gh", 'ਙ': "ng",
	'ਚ': "ch", 'ਛ': "chh", 'ਜ': "j", 'ਝ': "jh", 'ਞ': "ny",
	'ਟ': "t", 'ਠ': "th", 'ਡ': "d", 'ਢ': "dh", 'ਣ': "n",
	'ਤ': "t", 'ਥ': "th", 'ਦ': "d", 'ਧ': "dh", 'ਨ': "n",
	'ਪ': "p", 'ਫ': "ph", 'ਬ': "b", 'ਭ': "bh", 'ਮ': "m",
	'ਯ': "y", 'ਰ': "r", 'ਲ': "l", 'ਵ': "v", 'ੜ': "r",
	'ਸ': "s", 'ਹ': "h", 0x0A36: "sh", 0x0A5B: "z", 0x0A5E: "f",
}

var gurmukhiMatras = map[rune]string{
	'ਾ': "aa", 'ਿ': "i", 'ੀ': "ee", 'ੁ': "u", 'ੂ': "oo",
	'ੇ': "e", 'ੈ': "ai", 'ੋ': "o", 'ੌ': "au",
}

// TransliterateIndic transliterates Devanagari and Gurmukhi text into readable Latin script.
func TransliterateIndic(text string) string {
	script := DetectScript(text)
	if script == "latin" {
		return text
	}

	runesList := []rune(text)
	n := len(runesList)
	var sb strings.Builder

	for i := 0; i < n; i++ {
		r := runesList[i]

		// Devanagari
		if script == "devanagari" {
			if v, ok := devanagariVowels[r]; ok {
				sb.WriteString(v)
				continue
			}
			if c, ok := devanagariConsonants[r]; ok {
				sb.WriteString(c)
				// Look ahead for virama/halant (्), matra, or end of word
				if i+1 < n {
					next := runesList[i+1]
					if next == '्' { // virama: no inherent vowel
						i++ // skip virama
						continue
					}
					if m, ok := devanagariMatras[next]; ok {
						sb.WriteString(m)
						i++ // skip matra
						continue
					}
					if next == 'ं' || next == 'ँ' { // anusvara / chandrabindu
						sb.WriteString("an")
						i++
						continue
					}
					if unicode.IsSpace(next) || unicode.IsPunct(next) {
						// End of word in Hindi: schwa deletion
						continue
					}
					// Intermediate consonant without matra or virama: add inherent 'a'
					sb.WriteString("a")
				}
				continue
			}
			if m, ok := devanagariMatras[r]; ok {
				sb.WriteString(m)
				continue
			}
			if r == 'ं' || r == 'ँ' {
				sb.WriteString("n")
				continue
			}
			if r == '्' {
				continue
			}
		}

		// Gurmukhi
		if script == "gurmukhi" {
			if v, ok := gurmukhiVowels[r]; ok {
				sb.WriteString(v)
				continue
			}
			if c, ok := gurmukhiConsonants[r]; ok {
				sb.WriteString(c)
				if i+1 < n {
					next := runesList[i+1]
					if next == '੍' { // Gurmukhi halant
						i++
						continue
					}
					if m, ok := gurmukhiMatras[next]; ok {
						sb.WriteString(m)
						i++
						continue
					}
					if next == 'ੰ' || next == 'ਂ' { // tippi / bindi
						sb.WriteString("an")
						i++
						continue
					}
					if unicode.IsSpace(next) || unicode.IsPunct(next) {
						continue
					}
					sb.WriteString("a")
				}
				continue
			}
			if m, ok := gurmukhiMatras[r]; ok {
				sb.WriteString(m)
				continue
			}
			if r == 'ੰ' || r == 'ਂ' {
				sb.WriteString("n")
				continue
			}
			if r == 'ੱ' { // addak (gemination)
				if i+1 < n {
					next := runesList[i+1]
					if c, ok := gurmukhiConsonants[next]; ok {
						sb.WriteString(c)
					}
				}
				continue
			}
			if r == '੍' {
				continue
			}
		}

		// Fallback rune
		sb.WriteRune(r)
	}

	return strings.TrimSpace(sb.String())
}
