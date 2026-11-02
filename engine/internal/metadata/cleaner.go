package metadata

import (
	"crypto/sha1"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var (
	albumSlugRegex = regexp.MustCompile(`[^a-z0-9 ]+`)
	reDisc         = regexp.MustCompile(`(?i)[\(\[]\s*(?:disc|cd|tape|vol|volume)\s*\d+[\)\]]`)
	reDescriptors  = regexp.MustCompile(`(?i)[\(\[]\s*(?:official\s+(?:audio|video|music\s+video)|lyric(?:s)?\s+video|official|audio|video|mv|visualizer|remaster(?:ed)?(?:\s+\d+)?|hd|4k|explicit|clean|flac|320kbps|24bit|live(?:\s+at[^\]\)]+)?)\s*[\)\]]`)
	reStandalone   = regexp.MustCompile(`(?i)\b(?:official\s+(?:audio|video|music\s+video)|lyric(?:s)?\s+video|visualizer|remaster(?:ed)?(?:\s+\d+)?|explicit|clean)\b`)
	reChannel      = regexp.MustCompile(`(?i)@[\w_]+`)
	reSpaces       = regexp.MustCompile(`\s+`)
)

// Slugify converts text into a canonical lowercase alphanumeric slug with underscores.
// If non-Latin or empty, falls back to u_<sha1_8>.
func Slugify(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return ""
	}
	s := strings.ReplaceAll(raw, "÷", " divide ")
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.ReplaceAll(s, "+", " plus ")

	// NFKD normalization to ASCII
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)))
	ascii, _, _ := transform.String(t, s)

	// Keep only alphanumeric and space
	ascii = albumSlugRegex.ReplaceAllString(ascii, " ")
	tokens := strings.Fields(ascii)
	s = strings.Join(tokens, "_")
	s = strings.Trim(s, "_")

	if s != "" {
		return s
	}

	h := sha1.Sum([]byte(raw))
	return fmt.Sprintf("u_%x", h[:4])
}

// ArtistID returns the canonical artist ID: artist_<slug>.
func ArtistID(name string) string {
	s := Slugify(name)
	if s == "" {
		return ""
	}
	return "artist_" + s
}

// AlbumID returns the canonical album ID: album_<slug>_<year> or album_<slug>.
func AlbumID(album string, year int) string {
	b := Slugify(album)
	if b == "" {
		return ""
	}
	if year >= 1000 && year <= 2100 {
		return fmt.Sprintf("album_%s_%d", b, year)
	}
	return fmt.Sprintf("album_%s", b)
}

// NormalizePunctuation standardizes full-width and non-standard punctuation to ASCII equivalents.
func NormalizePunctuation(s string) string {
	if s == "" {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(s))

	for _, r := range s {
		switch r {
		case '，': // Full-width comma
			sb.WriteString(", ")
		case '／': // Full-width slash
			sb.WriteString(" / ")
		case '；': // Full-width semicolon
			sb.WriteString("; ")
		case '：': // Full-width colon
			sb.WriteString(": ")
		default:
			sb.WriteRune(r)
		}
	}

	// Normalize spaces
	fields := strings.Fields(sb.String())
	res := strings.Join(fields, " ")

	res = strings.ReplaceAll(res, " ,", ",")
	res = strings.ReplaceAll(res, " ;", ";")
	res = strings.ReplaceAll(res, ",,", ",")

	return strings.TrimSpace(res)
}

// StripNoise removes common spam watermarks, channel handles, and descriptors.
func StripNoise(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}

	// Remove disc/volume markers
	s = reDisc.ReplaceAllString(s, " ")

	// Remove official video, lyric video, remaster, audio descriptors
	s = reDescriptors.ReplaceAllString(s, " ")

	// Remove standalone noise words
	s = reStandalone.ReplaceAllString(s, " ")

	// Remove unclosed brackets
	if (strings.Contains(s, "(") && !strings.Contains(s, ")")) || (strings.Contains(s, "[") && !strings.Contains(s, "]")) {
		s = strings.ReplaceAll(s, "(", " ")
		s = strings.ReplaceAll(s, "[", " ")
	}

	// Remove telegram channel handles (@channel_name)
	s = reChannel.ReplaceAllString(s, " ")

	// Normalize spaces
	s = reSpaces.ReplaceAllString(s, " ")

	return strings.TrimSpace(s)
}

// CleanDuplicatedPhrase checks if a string consists of identical repeated halves
// separated by common delimiters (e.g., ", ", " / ", " - ", "; ").
func CleanDuplicatedPhrase(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 4 {
		return s, false
	}

	delimiters := []string{", ", " / ", " - ", "; ", " // "}

	// 1. Spaced delimiters
	for _, delim := range delimiters {
		parts := strings.Split(trimmed, delim)
		n := len(parts)
		if n >= 2 && n%2 == 0 {
			half := n / 2
			firstHalf := parts[:half]
			secondHalf := parts[half:]

			match := true
			for i := 0; i < half; i++ {
				p1 := strings.TrimSpace(firstHalf[i])
				p2 := strings.TrimSpace(secondHalf[i])
				if p1 == "" || !strings.EqualFold(p1, p2) {
					match = false
					break
				}
			}
			if match {
				cleaned := strings.Join(firstHalf, delim)
				return strings.TrimSpace(cleaned), true
			}
		}
	}

	// 2. Delimiters without spacing
	delimsNoSpace := []string{",", "/", ";"}
	for _, delim := range delimsNoSpace {
		parts := strings.Split(trimmed, delim)
		n := len(parts)
		if n >= 2 && n%2 == 0 {
			half := n / 2
			firstHalf := parts[:half]
			secondHalf := parts[half:]

			match := true
			for i := 0; i < half; i++ {
				p1 := strings.TrimSpace(firstHalf[i])
				p2 := strings.TrimSpace(secondHalf[i])
				if p1 == "" || !strings.EqualFold(p1, p2) {
					match = false
					break
				}
			}
			if match {
				cleaned := strings.Join(firstHalf, delim+" ")
				return strings.TrimSpace(cleaned), true
			}
		}
	}

	return s, false
}

// CleanArtist deduplicates stuttered artist tags (e.g. "Shaan, Kailash Kher, Shaan, Kailash Kher").
func CleanArtist(artist string) (string, bool) {
	norm := NormalizePunctuation(artist)
	cleaned, changed := CleanDuplicatedPhrase(norm)
	if changed {
		return StripNoise(cleaned), true
	}
	return StripNoise(norm), false
}

// CleanTitle deduplicates stuttered track titles with heuristics to preserve intentional repetitions.
func CleanTitle(title string, artist string) (string, bool) {
	norm := NormalizePunctuation(title)
	candidate, changed := CleanDuplicatedPhrase(norm)
	if !changed {
		return StripNoise(norm), false
	}

	_, artistChanged := CleanArtist(artist)
	if artistChanged {
		return StripNoise(candidate), true
	}

	words := strings.Fields(candidate)
	if len(words) >= 3 || len(candidate) >= 15 {
		return StripNoise(candidate), true
	}

	return StripNoise(norm), false
}

// CleanMetadata cleans and strips noise from both title and artist tags.
func CleanMetadata(title, artist string) (cleanTitle string, cleanArtist string) {
	cleanArtist, _ = CleanArtist(artist)
	cleanTitle, _ = CleanTitle(title, artist)
	if cleanArtist == "" {
		cleanArtist = StripNoise(artist)
	}
	if cleanTitle == "" {
		cleanTitle = StripNoise(title)
	}
	return cleanTitle, cleanArtist
}

var reTrackPrefix = regexp.MustCompile(`^(?:[0-9]{1,3}[.\-_\s]+)`)

func stripTrackNumberPrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	cleaned := reTrackPrefix.ReplaceAllString(trimmed, "")
	return strings.TrimSpace(cleaned)
}

// ExtractTitleAndArtist intelligently resolves clean title and artist using existing metadata,
// DocumentAttributeAudio tags, and Telegram document filename patterns.
func ExtractTitleAndArtist(rawTitle, rawArtist, rawFileName string) (string, string) {
	title := stripTrackNumberPrefix(strings.TrimSpace(rawTitle))
	artist := strings.TrimSpace(rawArtist)
	if strings.EqualFold(artist, "Unknown Artist") {
		artist = ""
	}

	// 1. If artist is missing or title contains " - ", examine rawFileName and rawTitle
	candidateStr := ""
	if rawFileName != "" {
		ext := filepath.Ext(rawFileName)
		candidateStr = strings.TrimSuffix(rawFileName, ext)
		// If filename uses underscores instead of spaces, convert them
		if strings.Contains(candidateStr, "_") && !strings.Contains(candidateStr, " ") {
			candidateStr = strings.ReplaceAll(candidateStr, "_", " ")
		}
	}

	// If candidateStr is empty, use rawTitle as candidate
	if candidateStr == "" && strings.Contains(title, " - ") {
		candidateStr = title
	}

	// Strip track numbers like "01. ", "01 - ", "1 " at the beginning
	candidateStr = stripTrackNumberPrefix(candidateStr)

	// If we have a candidate string with a separator (" - ", " – ", " — ")
	sep := ""
	if strings.Contains(candidateStr, " - ") {
		sep = " - "
	} else if strings.Contains(candidateStr, " – ") {
		sep = " – "
	} else if strings.Contains(candidateStr, " — ") {
		sep = " — "
	}

	if sep != "" {
		parts := strings.SplitN(candidateStr, sep, 2)
		p1 := strings.TrimSpace(parts[0])
		p2 := strings.TrimSpace(parts[1])

		if p1 != "" && p2 != "" {
			// Case A: artist is already known; see which part matches title or artist
			if artist != "" {
				if title == "" || strings.EqualFold(title, candidateStr) {
					// candidateStr has both title and artist
					if strings.Contains(strings.ToLower(p2), strings.ToLower(artist)) || strings.Contains(strings.ToLower(artist), strings.ToLower(p2)) {
						title = p1
					} else if strings.Contains(strings.ToLower(p1), strings.ToLower(artist)) || strings.Contains(strings.ToLower(artist), strings.ToLower(p1)) {
						title = p2
					} else {
						title = p1
					}
				}
			} else {
				// Case B: artist is missing.
				// If title is known (e.g. from DocumentAttributeAudio.Title):
				if title != "" && !strings.EqualFold(title, candidateStr) {
					lowerT := strings.ToLower(title)
					if strings.Contains(lowerT, strings.ToLower(p1)) || strings.Contains(strings.ToLower(p1), lowerT) {
						artist = p2
					} else if strings.Contains(lowerT, strings.ToLower(p2)) || strings.Contains(strings.ToLower(p2), lowerT) {
						artist = p1
						title = p2
					} else {
						artist = p2
					}
				} else {
					// Neither artist is known, or title was just the raw filename:
					// Standard Telegram convention: Title - Artist
					title = p1
					artist = p2
				}
			}
		}
	}

	// Final cleanup
	cleanTitle, cleanArtist := CleanMetadata(title, artist)
	if cleanTitle == "" {
		cleanTitle = StripNoise(title)
	}
	if cleanTitle == "" && rawFileName != "" {
		cleanTitle = strings.TrimSuffix(rawFileName, filepath.Ext(rawFileName))
	}
	if cleanArtist == "" {
		cleanArtist = "Unknown Artist"
	}

	return cleanTitle, cleanArtist
}

