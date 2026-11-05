package services

import (
	"math"
	"strings"
	"unicode"

	"musix/engine/internal/metadata"
)

// CoverCandidate represents a potential cover-art match returned by an external provider.
type CoverCandidate struct {
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	Album       string   `json:"album"`
	Year        int      `json:"year"`
	DurationSec int      `json:"duration_sec"`
	ImageURL    string   `json:"image_url"`
	Language    string   `json:"language,omitempty"`
	Provider    string   `json:"provider"`
}

// CandidateScore holds the evaluation result for a candidate.
type CandidateScore struct {
	Score     float64 `json:"score"`
	Rule      string  `json:"rule"`
	Reason    string  `json:"reason"`
	Candidate CoverCandidate
}

// ScoreCandidate evaluates a candidate cover against local structured track metadata.
// Returns a normalized score in [0.0, 1.0], rule name, and diagnostic reason.
// If score is below acceptance threshold or rejected by hard rule, score is returned as 0.0.
func ScoreCandidate(local metadata.StructuredMetadata, localDurationSec int, cand CoverCandidate, runnerUpScore ...float64) (float64, string, string) {
	if cand.ImageURL == "" {
		return 0, "reject", "missing image url"
	}

	// 1. Duration Check: hard reject if difference > 10 seconds (best Indian music disambiguator)
	var durationBonus float64
	hasDurationCheck := localDurationSec > 0 && cand.DurationSec > 0
	var durationDiff int
	if hasDurationCheck {
		durationDiff = int(math.Abs(float64(localDurationSec - cand.DurationSec)))
		if durationDiff > 10 {
			return 0, "hard_reject_duration", "duration diff > 10s"
		}
		if durationDiff <= 3 {
			durationBonus = 0.25
		} else if durationDiff <= 6 {
			durationBonus = 0.15
		}
	}

	// 2. Title similarity
	targetTitle := local.SearchTitle
	if targetTitle == "" {
		targetTitle = local.Title
	}
	lowerTargetTitle := strings.ToLower(targetTitle)
	if strings.HasPrefix(lowerTargetTitle, "track ") || strings.HasPrefix(lowerTargetTitle, "audio ") || lowerTargetTitle == "track" || lowerTargetTitle == "audio" {
		return 0, "reject_placeholder_title", "placeholder title cannot match cover"
	}
	titleSim := TitleSimilarity(targetTitle, cand.Title)
	if titleSim < 0.40 {
		return 0, "reject_title", "title similarity too low"
	}

	// 3. Artist similarity
	var artistSim float64
	hasLocalArtist := len(local.SearchArtists) > 0 || len(local.Artists) > 0
	if hasLocalArtist {
		searchArtists := local.SearchArtists
		if len(searchArtists) == 0 {
			searchArtists = local.Artists
		}
		artistSim = ArtistOverlap(searchArtists, cand.Artists)
		if artistSim < 0.25 {
			return 0, "reject_artist", "artist mismatch"
		}
	}

	// 4. Album / Movie hint match
	var albumBonus float64
	hasAlbumMatch := false
	if local.AlbumHint != "" && cand.Album != "" {
		if TokenSetOverlap(local.AlbumHint, cand.Album) >= 0.60 {
			albumBonus = 0.20
			hasAlbumMatch = true
		}
	}

	// 5. Version tag mismatch penalty
	// If candidate is a remix/lofi/slowed/cover/karaoke but local track is original, reject or penalize heavily
	candTags := ExtractVersionTagsFromText(cand.Title + " " + cand.Album)
	var tagPenalty float64
	var tagBonus float64
	hasLocalVersion := len(local.VersionTags) > 0
	hasCandVersion := len(candTags) > 0

	if hasCandVersion && !hasLocalVersion {
		// Local didn't specify remix/lofi, but candidate is a remix/lofi
		return 0, "reject_version", "unwanted remix/lofi/cover version"
	} else if hasLocalVersion && hasCandVersion {
		// Both have version tags - verify overlap
		if HasTagOverlap(local.VersionTags, candTags) {
			tagBonus = 0.15
		} else {
			tagPenalty = 0.35
		}
	} else if hasLocalVersion && !hasCandVersion {
		// Local is remix/lofi but candidate is original audio
		tagPenalty = 0.30
	}

	// 6. Year proximity bonus
	var yearBonus float64
	if cand.Year > 0 {
		yearBonus = 0.05
	}

	// 7. Calculate composite score
	var composite float64
	if hasLocalArtist {
		composite = (0.45 * titleSim) + (0.35 * artistSim) + durationBonus + albumBonus + yearBonus + tagBonus - tagPenalty
	} else {
		// Local artist is missing
		composite = (0.65 * titleSim) + durationBonus + albumBonus + yearBonus + tagBonus - tagPenalty
	}

	if composite > 1.0 {
		composite = 1.0
	}
	if composite < 0.0 {
		composite = 0.0
	}

	// 8. Acceptance threshold
	if hasLocalArtist {
		if composite < 0.70 {
			return 0, "below_threshold", "composite score below 0.70"
		}
		rule := "exact_match"
		if composite < 0.85 {
			rule = "high_confidence"
		}
		return composite, rule, "accepted"
	}

	// Missing local artist path: require duration match + album match OR clear score margin (Prompt 4 requirement)
	if hasDurationCheck && durationDiff <= 5 && hasAlbumMatch && composite >= 0.75 {
		return composite, "missing_artist_duration_album", "accepted with duration and album match"
	}

	var runnerUp float64
	if len(runnerUpScore) > 0 {
		runnerUp = runnerUpScore[0]
	}
	if composite >= 0.82 && (runnerUp == 0.0 || (composite-runnerUp) >= 0.20) {
		return composite, "missing_artist_margin", "accepted with high score margin"
	}

	return 0, "below_threshold_missing_artist", "missing artist requires duration+album match or score margin"
}

// TokenSetOverlap computes Jaccard similarity of normalized alphanumeric words between s1 and s2.
func TokenSetOverlap(s1, s2 string) float64 {
	tokens1 := tokenize(s1)
	tokens2 := tokenize(s2)
	if len(tokens1) == 0 || len(tokens2) == 0 {
		return 0.0
	}

	set1 := make(map[string]struct{}, len(tokens1))
	for _, t := range tokens1 {
		set1[t] = struct{}{}
	}

	set2 := make(map[string]struct{}, len(tokens2))
	for _, t := range tokens2 {
		set2[t] = struct{}{}
	}

	var intersection int
	for t := range set1 {
		if _, exists := set2[t]; exists {
			intersection++
		}
	}

	union := len(set1) + len(set2) - intersection
	if union <= 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}

// JaroWinkler calculates the Jaro-Winkler distance between two strings in [0.0, 1.0].
func JaroWinkler(s1, s2 string) float64 {
	s1 = strings.ToLower(strings.TrimSpace(s1))
	s2 = strings.ToLower(strings.TrimSpace(s2))
	if s1 == s2 {
		return 1.0
	}
	if len(s1) == 0 || len(s2) == 0 {
		return 0.0
	}

	r1 := []rune(s1)
	r2 := []rune(s2)
	l1 := len(r1)
	l2 := len(r2)

	matchDistance := (int(math.Max(float64(l1), float64(l2))) / 2) - 1
	if matchDistance < 0 {
		matchDistance = 0
	}

	s1Matches := make([]bool, l1)
	s2Matches := make([]bool, l2)

	var matches int
	for i := 0; i < l1; i++ {
		start := int(math.Max(0, float64(i-matchDistance)))
		end := int(math.Min(float64(i+matchDistance+1), float64(l2)))

		for j := start; j < end; j++ {
			if s2Matches[j] || r1[i] != r2[j] {
				continue
			}
			s1Matches[i] = true
			s2Matches[j] = true
			matches++
			break
		}
	}

	if matches == 0 {
		return 0.0
	}

	var transpositions int
	k := 0
	for i := 0; i < l1; i++ {
		if !s1Matches[i] {
			continue
		}
		for !s2Matches[k] {
			k++
		}
		if r1[i] != r2[k] {
			transpositions++
		}
		k++
	}

	m := float64(matches)
	jaro := ((m / float64(l1)) + (m / float64(l2)) + ((m - float64(transpositions/2)) / m)) / 3.0

	// Winkler prefix bonus (up to 4 matching initial runes)
	prefixLen := 0
	maxPrefix := int(math.Min(4, math.Min(float64(l1), float64(l2))))
	for i := 0; i < maxPrefix; i++ {
		if r1[i] == r2[i] {
			prefixLen++
		} else {
			break
		}
	}

	return jaro + (float64(prefixLen) * 0.1 * (1.0 - jaro))
}

// TitleSimilarity combines token-set overlap and Jaro-Winkler string similarity.
func TitleSimilarity(t1, t2 string) float64 {
	clean1 := strings.ToLower(metadata.StripNoise(t1))
	clean2 := strings.ToLower(metadata.StripNoise(t2))
	if clean1 == clean2 && clean1 != "" {
		return 1.0
	}

	tokenScore := TokenSetOverlap(clean1, clean2)
	jwScore := JaroWinkler(clean1, clean2)

	// Weighted blend
	return (0.60 * tokenScore) + (0.40 * jwScore)
}

// ArtistOverlap checks whether local artists match candidate artists, with partial name tolerance.
// (e.g. "Arijit" matches "Arijit Singh", "Pritam" matches "Pritam Chakraborty").
func ArtistOverlap(localArtists, candArtists []string) float64 {
	if len(localArtists) == 0 || len(candArtists) == 0 {
		return 0.0
	}

	var totalScore float64
	for _, la := range localArtists {
		cleanLocal := strings.ToLower(metadata.StripNoise(la))
		if cleanLocal == "" {
			continue
		}
		localTokens := tokenize(cleanLocal)

		var bestMatch float64
		for _, ca := range candArtists {
			cleanCand := strings.ToLower(metadata.StripNoise(ca))
			if cleanCand == "" {
				continue
			}
			if cleanLocal == cleanCand {
				bestMatch = 1.0
				break
			}

			// Check substring or token subset
			if strings.Contains(cleanCand, cleanLocal) || strings.Contains(cleanLocal, cleanCand) {
				if bestMatch < 0.90 {
					bestMatch = 0.90
				}
			}

			// Partial name tolerance: check individual name tokens
			candTokens := tokenize(cleanCand)
			for _, lt := range localTokens {
				if len(lt) < 3 {
					continue
				}
				for _, ct := range candTokens {
					if lt == ct {
						if bestMatch < 0.85 {
							bestMatch = 0.85
						}
					}
				}
			}

			jw := JaroWinkler(cleanLocal, cleanCand)
			if jw > bestMatch {
				bestMatch = jw
			}
		}
		totalScore += bestMatch
	}

	return totalScore / float64(len(localArtists))
}

// ExtractVersionTagsFromText identifies version descriptors in candidate strings.
func ExtractVersionTagsFromText(text string) []string {
	lower := strings.ToLower(text)
	var tags []string
	knownVersions := []string{
		"remix", "lofi", "lo-fi", "slowed", "reverb", "unplugged",
		"reprise", "cover", "karaoke", "instrumental", "acoustic",
		"live", "mashup", "female version", "male version",
	}
	for _, v := range knownVersions {
		if strings.Contains(lower, v) {
			cleanTag := strings.ReplaceAll(v, "-", "")
			cleanTag = strings.ReplaceAll(cleanTag, " ", "")
			tags = append(tags, cleanTag)
		}
	}
	return tags
}

// HasTagOverlap returns true if any version tags match.
func HasTagOverlap(t1, t2 []string) bool {
	for _, a := range t1 {
		normA := strings.ToLower(strings.ReplaceAll(a, "-", ""))
		for _, b := range t2 {
			normB := strings.ToLower(strings.ReplaceAll(b, "-", ""))
			if normA == normB {
				return true
			}
		}
	}
	return false
}

func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
