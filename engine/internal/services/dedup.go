package services

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

var reNonAlpha = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// NormalizeText cleans and strips non-alphanumeric characters for fingerprinting (supports multilingual Unicode).
func NormalizeText(text string) string {
	s := strings.ToLower(text)
	s = reNonAlpha.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// MetadataFingerprint generates a fuzzy bucketed fingerprint: normalized_title|normalized_artist|normalized_album|duration_bucket.
// Duration is bucketed to the nearest 2 seconds to tolerate slight encoding differences.
func MetadataFingerprint(title, artist, album string, durationSec int) string {
	t := NormalizeText(title)
	a := NormalizeText(artist)
	al := NormalizeText(album)

	durKey := ""
	if durationSec > 0 {
		bucket := int64(math.Round(float64(durationSec)/2.0) * 2)
		durKey = fmt.Sprintf("%d", bucket)
	}

	parts := []string{t, a, al, durKey}
	return strings.Trim(strings.Join(parts, "|"), "|")
}

// FindDuplicate checks SQLite database to find if a track already exists:
// 1. Exact Telegram document ID (file_unique_id) in track_sources.
// 2. Exact match on fuzzy metadata fingerprint.
// 3. Conservative match on (normalized_title + normalized_artist + duration +-2s).
func FindDuplicate(
	ctx context.Context,
	db *database.DB,
	fileUniqueID, title, artist, album string,
	durationSec int,
) (*models.Track, bool, error) {
	if db == nil {
		return nil, false, nil
	}

	// 1. Check exact Telegram file unique ID
	if fileUniqueID != "" {
		if t, err := db.FindTrackByFileUniqueID(ctx, fileUniqueID); err == nil && t != nil {
			return t, true, nil
		}
	}

	// 2. Check metadata fingerprint
	fp := MetadataFingerprint(title, artist, album, durationSec)
	if fp != "" {
		if t, err := db.FindTrackByFingerprint(ctx, fp); err == nil && t != nil {
			return t, true, nil
		}
	}

	// 3. Conservative fuzzy match on title + artist + duration +-2s
	if title != "" && artist != "" {
		if t, err := db.FindTrackByFuzzy(ctx, title, artist, durationSec); err == nil && t != nil {
			return t, true, nil
		}
	}

	return nil, false, nil
}

