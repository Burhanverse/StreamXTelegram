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
	albumSlugRegex    = regexp.MustCompile(`[^a-z0-9 ]+`)
	reDisc            = regexp.MustCompile(`(?i)[\(\[]\s*(?:disc|cd|tape|vol|volume)\s*\d+[\)\]]`)
	reSiteBrackets    = regexp.MustCompile(`(?i)[\(\[]\s*[^\]\)]*\b(?:songspk|pagalworld|mr-jatt|mrjatt|djmaza|djpunjab|downloadming|pagalfree|naasongs|masstamilan|sensongs|kuttyweb|pendujatt|mp3tau|freshmaza|wapking)\b[^\]\)]*[\)\]]`)
	reBitrateBrackets = regexp.MustCompile(`(?i)[\(\[]\s*(?:[_\-\s]*)(?:128|192|256|320)\s*(?:kbps|k)?\s*[\)\]]`)
	reSites           = regexp.MustCompile(`(?i)\b[a-zA-Z0-9_\-]*(?:songspk|pagalworld|mr-jatt|mrjatt|djmaza|djpunjab|downloadming|pagalfree|naasongs|masstamilan|sensongs|kuttyweb|pendujatt|mp3tau|freshmaza|wapking)[a-zA-Z0-9_\-\.]*\b`)
	reDomains         = regexp.MustCompile(`(?i)\b(?:www\.)?[a-zA-Z0-9_\-]+\.(?:com|in|net|org|vip|me|co|cc|info|fm|link|site|xyz|club)\b`)
	reBitrates        = regexp.MustCompile(`(?i)(?:[_\-\s]+|\b)(?:128|192|256|320)\s*(?:kbps|k)?\b`)
	reDescriptors     = regexp.MustCompile(`(?i)[\(\[]\s*(?:official\s+(?:audio|video|music\s+video)|lyric(?:s)?\s+(?:video|audio)?|official|audio|video|mv|visualizer|remaster(?:ed)?(?:\s+\d+)?|hd|4k|explicit|clean|flac|320kbps|128kbps|192kbps|256kbps|24bit|live(?:\s+at[^\]\)]+)?|full\s+(?:audio\s+)?song|video\s+song|full\s+video\s+song|teaser|promo|ost)\s*[\)\]]`)
	reStandalone      = regexp.MustCompile(`(?i)\b(?:official\s+(?:audio|video|music\s+video)|lyric(?:s)?\s+(?:video|audio)?|visualizer|remaster(?:ed)?(?:\s+\d+)?|explicit|clean|full\s+(?:audio\s+)?song|video\s+song|full\s+video\s+song|audio\s+song)\b`)
	reEmptyBrackets   = regexp.MustCompile(`[\(\[]\s*[\)\]]`)
	reTrailingPunct   = regexp.MustCompile(`\s*[-–—/,\.]+\s*$`)
	reLeadingPunct    = regexp.MustCompile(`^\s*[-–—/,\.]+\s*`)
	reVersionInParens = regexp.MustCompile(`(?i)\s*[\(\[]\s*(?:[a-zA-Z0-9\+\s]*\b(?:remix|lofi|slowed|reverb|unplugged|reprise|cover|karaoke|instrumental|live|acoustic|club mix|mashup)\b[a-zA-Z0-9\+\s]*)\s*[\)\]]`)
	reMovieHint       = regexp.MustCompile(`(?i)[\(\[]\s*(?:From|From\s+The\s+Movie|Film)\s*["“']?([^"”'\]\)]+)["”']?\s*[\)\]]`)
	reTrackPrefix     = regexp.MustCompile(`^(?:[0-9]{1,2}[.\-_]+|[0-9]{2}\s+)`)
	reChannel         = regexp.MustCompile(`(?i)@[\w_]+`)
	reSpaces          = regexp.MustCompile(`\s+`)
)

// StructuredMetadata contains fully normalized, script-aware metadata extracted from Indian music uploads.
type StructuredMetadata struct {
	Title          string   `json:"title"`
	SearchTitle    string   `json:"search_title"`
	Artists        []string `json:"artists"`
	SearchArtists  []string `json:"search_artists"`
	AlbumHint      string   `json:"album_hint"`
	VersionTags    []string `json:"version_tags"`
	LanguageScript string   `json:"language_script"`
	Confidence     float64  `json:"confidence"`
}

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
	return AlbumIDWithArtist(album, "", year)
}

// AlbumIDWithArtist returns the canonical album ID incorporating album artist to prevent
// unrelated albums with generic titles like "Single" or "Soundtrack" from merging.
func AlbumIDWithArtist(album, artist string, year int) string {
	b := Slugify(album)
	if b == "" {
		return ""
	}
	var parts []string
	parts = append(parts, "album", b)
	if a := Slugify(artist); a != "" && a != "unknown-artist" && a != "various-artists" {
		parts = append(parts, a)
	}
	if year >= 1000 && year <= 2100 {
		parts = append(parts, fmt.Sprintf("%d", year))
	}
	return strings.Join(parts, "_")
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

// StripNoise removes common spam watermarks, channel handles, bitrates, and video descriptors.
func StripNoise(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}

	// 1. Unicode NFKC normalization and zero-width stripping
	s = norm.NFKC.String(s)
	s = strings.ReplaceAll(s, "\u200B", "")
	s = strings.ReplaceAll(s, "\u200C", "")
	s = strings.ReplaceAll(s, "\u200D", "")
	s = strings.ReplaceAll(s, "\uFEFF", "")

	// 2. Remove site tags in brackets ([SongsPK], [DJMaza], etc.) and bitrates in brackets
	s = reSiteBrackets.ReplaceAllString(s, " ")
	s = reBitrateBrackets.ReplaceAllString(s, " ")

	// 3. Remove standalone site names and domains
	s = reSites.ReplaceAllString(s, " ")
	s = reDomains.ReplaceAllString(s, " ")

	// 4. Remove bitrates (128 kbps, 192Kbps, 320, - 320)
	s = reBitrates.ReplaceAllString(s, " ")

	// 5. Remove disc/volume markers
	s = reDisc.ReplaceAllString(s, " ")

	// 6. Remove official video, lyric video, remaster, audio descriptors
	s = reDescriptors.ReplaceAllString(s, " ")

	// 7. Remove standalone noise words
	s = reStandalone.ReplaceAllString(s, " ")

	// 8. Remove empty brackets
	s = reEmptyBrackets.ReplaceAllString(s, " ")

	// 9. Remove unclosed brackets
	if (strings.Contains(s, "(") && !strings.Contains(s, ")")) || (strings.Contains(s, "[") && !strings.Contains(s, "]")) {
		s = strings.ReplaceAll(s, "(", " ")
		s = strings.ReplaceAll(s, "[", " ")
	}

	// 10. Remove telegram channel handles (@channel_name)
	s = reChannel.ReplaceAllString(s, " ")

	// 11. Normalize spaces and strip dangling punctuation
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = reTrailingPunct.ReplaceAllString(s, "")
	s = reLeadingPunct.ReplaceAllString(s, "")
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

func stripTrackNumberPrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	cleaned := reTrackPrefix.ReplaceAllString(trimmed, "")
	return strings.TrimSpace(cleaned)
}

var knownPopularArtists = []string{
	"arijit singh", "shreya ghoshal", "atif aslam", "neha kakkar", "sonu nigam",
	"sunidhi chauhan", "jubin nautiyal", "badshah", "ap dhillon", "diljit dosanjh",
	"sidhu moose wala", "karan aujla", "yo yo honey singh", "honey singh", "pritam",
	"a.r. rahman", "ar rahman", "vishal mishra", "darshan raval", "armaan malik",
	"kumar sanu", "alka yagnik", "udit narayan", "kishore kumar", "lata mangeshkar",
	"mohammed rafi", "anirudh ravichander", "devi sri prasad", "sid sriram",
	"guru randhawa", "b praak", "jasleen royal", "stebin ben", "ankit tiwari",
	"mohit chauhan", "kk", "shaquille", "kailash kher", "shaan", "himesh reshammiya",
	"mika singh", "rahat fateh ali khan", "nusrat fateh ali khan", "pankaj udhas",
	"jagjit singh", "arijit", "shreya", "diljit", "sidhu",
}

func isLikelyArtist(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, a := range knownPopularArtists {
		if strings.EqualFold(lower, a) || strings.HasPrefix(lower, a+" ") || strings.HasSuffix(lower, " "+a) {
			return true
		}
	}
	return false
}

var versionTagKeywords = []string{
	"remix", "lofi", "slowed", "reverb", "unplugged", "reprise", "cover",
	"karaoke", "instrumental", "live", "acoustic", "club mix", "dj mix", "mashup",
}

func extractVersionTags(s string) []string {
	lower := strings.ToLower(s)
	var tags []string
	seen := make(map[string]bool)

	for _, kw := range versionTagKeywords {
		if strings.Contains(lower, kw) && !seen[kw] {
			tags = append(tags, kw)
			seen[kw] = true
		}
	}
	return tags
}

func extractMovieHint(s string) (cleanStr string, movie string) {
	if m := reMovieHint.FindStringSubmatch(s); len(m) > 1 {
		movie = strings.TrimSpace(m[1])
		cleanStr = reMovieHint.ReplaceAllString(s, " ")
		return strings.TrimSpace(cleanStr), movie
	}
	return s, ""
}

// SplitAndCleanArtists splits multiple artists, removes placeholders/noise, and dedupes.
func SplitAndCleanArtists(raw string) []string {
	clean := StripNoise(NormalizePunctuation(raw))
	if clean == "" || strings.EqualFold(clean, "Unknown Artist") {
		return nil
	}

	replacer := strings.NewReplacer(
		" / ", ",",
		" & ", ",",
		" and ", ",",
		" ft. ", ",",
		" feat. ", ",",
		" feat ", ",",
		" ft ", ",",
		" x ", ",",
		" X ", ",",
		" with ", ",",
		";", ",",
	)
	normalized := replacer.Replace(clean)
	parts := strings.Split(normalized, ",")

	var artists []string
	seen := make(map[string]bool)

	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		trimmed = strings.TrimPrefix(trimmed, "Composer:")
		trimmed = strings.TrimPrefix(trimmed, "Singer:")
		trimmed = strings.TrimSpace(trimmed)

		if len(trimmed) < 2 || strings.EqualFold(trimmed, "Unknown Artist") {
			continue
		}

		lower := strings.ToLower(trimmed)
		if lower == "various artists" || lower == "various" {
			continue
		}

		if !seen[lower] {
			seen[lower] = true
			artists = append(artists, trimmed)
		}
	}

	return artists
}

// ParseStructuredMetadata extracts high-confidence structured metadata with Indic script awareness.
func ParseStructuredMetadata(rawTitle, rawArtist, rawFileName string) StructuredMetadata {
	title := stripTrackNumberPrefix(strings.TrimSpace(rawTitle))
	artist := strings.TrimSpace(rawArtist)
	if strings.EqualFold(artist, "Unknown Artist") {
		artist = ""
	}

	// 1. Extract album hint from From "Movie" in title or filename
	var albumHint string
	if cleanT, hint := extractMovieHint(title); hint != "" {
		albumHint = hint
		title = cleanT
	}
	if albumHint == "" && rawFileName != "" {
		if _, hint := extractMovieHint(rawFileName); hint != "" {
			albumHint = hint
		}
	}

	// 2. Resolve filename and candidate strings
	candidateStr := ""
	if rawFileName != "" {
		ext := filepath.Ext(rawFileName)
		candidateStr = strings.TrimSuffix(rawFileName, ext)
		if strings.Contains(candidateStr, "_") && !strings.Contains(candidateStr, " ") {
			candidateStr = strings.ReplaceAll(candidateStr, "_", " ")
		}
	}
	if candidateStr == "" && strings.Contains(title, " - ") {
		candidateStr = title
	}
	candidateStr = stripTrackNumberPrefix(candidateStr)

	confidence := 0.5

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
			// Case A: artist is already provided from tags
			if artist != "" {
				if title == "" || strings.EqualFold(title, candidateStr) {
					if strings.Contains(strings.ToLower(p2), strings.ToLower(artist)) || strings.Contains(strings.ToLower(artist), strings.ToLower(p2)) {
						title = p1
						confidence = 0.9
					} else if strings.Contains(strings.ToLower(p1), strings.ToLower(artist)) || strings.Contains(strings.ToLower(artist), strings.ToLower(p1)) {
						title = p2
						confidence = 0.9
					} else {
						title = p1
						confidence = 0.6
					}
				} else {
					confidence = 0.95
				}
			} else {
				// Case B: artist is missing - check known popular artists or known title tag
				p1IsArtist := isLikelyArtist(p1)
				p2IsArtist := isLikelyArtist(p2)

				if p1IsArtist && !p2IsArtist {
					// Filename order: Artist - Title
					artist = p1
					title = p2
					confidence = 0.85
				} else if p2IsArtist && !p1IsArtist {
					// Filename order: Title - Artist
					title = p1
					artist = p2
					confidence = 0.85
				} else if title != "" && !strings.EqualFold(title, candidateStr) {
					lowerT := strings.ToLower(title)
					if strings.Contains(lowerT, strings.ToLower(p1)) || strings.Contains(strings.ToLower(p1), lowerT) {
						artist = p2
						confidence = 0.85
					} else if strings.Contains(lowerT, strings.ToLower(p2)) || strings.Contains(strings.ToLower(p2), lowerT) {
						artist = p1
						title = p2
						confidence = 0.85
					} else {
						artist = p2
						confidence = 0.6
					}
				} else {
					// Fallback default: Title - Artist
					title = p1
					artist = p2
					confidence = 0.5
				}
			}
		}
	} else if title == "" && rawFileName != "" {
		title = candidateStr
		confidence = 0.3
	}

	// 3. Extract version tags (remix, lofi, slowed, etc.)
	versionTags := extractVersionTags(candidateStr + " " + title)
	if len(versionTags) > 0 {
		title = reVersionInParens.ReplaceAllString(title, "")
	}

	// 4. Final cleaning and noise stripping
	cleanTitle, _ := CleanMetadata(title, artist)
	if cleanTitle == "" {
		cleanTitle = StripNoise(title)
	}
	if len(versionTags) > 0 {
		cleanTitle = reVersionInParens.ReplaceAllString(cleanTitle, "")
		cleanTitle = StripNoise(cleanTitle)
	}
	if cleanTitle == "" && rawFileName != "" {
		cleanTitle = strings.TrimSuffix(rawFileName, filepath.Ext(rawFileName))
	}

	artistsList := SplitAndCleanArtists(artist)

	// 5. Detect script and generate Latin search variants
	script := DetectScript(cleanTitle)
	searchTitle := cleanTitle
	if script != "latin" {
		searchTitle = TransliterateIndic(cleanTitle)
	}

	var searchArtists []string
	for _, a := range artistsList {
		if DetectScript(a) != "latin" {
			searchArtists = append(searchArtists, TransliterateIndic(a))
		} else {
			searchArtists = append(searchArtists, a)
		}
	}

	return StructuredMetadata{
		Title:          cleanTitle,
		SearchTitle:    searchTitle,
		Artists:        artistsList,
		SearchArtists:  searchArtists,
		AlbumHint:      albumHint,
		VersionTags:    versionTags,
		LanguageScript: script,
		Confidence:     confidence,
	}
}

// CleanMetadataStructured returns StructuredMetadata including album hint.
func CleanMetadataStructured(rawTitle, rawArtist, rawAlbum, rawFileName string) StructuredMetadata {
	sm := ParseStructuredMetadata(rawTitle, rawArtist, rawFileName)
	if sm.AlbumHint == "" && rawAlbum != "" && rawAlbum != "Telegram Music" {
		sm.AlbumHint = StripNoise(rawAlbum)
	}
	return sm
}

// ExtractTitleAndArtist wraps ParseStructuredMetadata for backward compatibility with existing callers.
func ExtractTitleAndArtist(rawTitle, rawArtist, rawFileName string) (string, string) {
	sm := ParseStructuredMetadata(rawTitle, rawArtist, rawFileName)
	artistStr := "Unknown Artist"
	if len(sm.Artists) > 0 {
		artistStr = strings.Join(sm.Artists, ", ")
	}
	return sm.Title, artistStr
}

