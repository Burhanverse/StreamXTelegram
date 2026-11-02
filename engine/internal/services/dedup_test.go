package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

func TestNormalizeText(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Queen - Bohemian Rhapsody!", "queen bohemian rhapsody"},
		{"AC/DC & The Band", "ac dc the band"},
		{"Beyoncé Knowles", "beyoncé knowles"}, // Multilingual unicode preserved
		{"", ""},
	}

	for _, tt := range tests {
		got := NormalizeText(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeText(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMetadataFingerprintBucketing(t *testing.T) {
	// Durations 181s and 182s bucket to the exact same 2-second bucket (182)
	fp1 := MetadataFingerprint("Imagine", "John Lennon", "Imagine", 181)
	fp2 := MetadataFingerprint("Imagine", "John Lennon", "Imagine", 182)

	if fp1 != fp2 {
		t.Errorf("Fingerprints did not bucket together: fp1=%q, fp2=%q", fp1, fp2)
	}

	// 183s and 184s bucket to the next 2-second bucket (184)
	fp3 := MetadataFingerprint("Imagine", "John Lennon", "Imagine", 183)
	fp4 := MetadataFingerprint("Imagine", "John Lennon", "Imagine", 184)
	if fp3 != fp4 {
		t.Errorf("Fingerprints did not bucket together: fp3=%q, fp4=%q", fp3, fp4)
	}

	// Different title should produce different fingerprint
	fpDiff := MetadataFingerprint("Jealous Guy", "John Lennon", "Imagine", 182)
	if fpDiff == fp1 {
		t.Errorf("Different tracks produced identical fingerprint: %q", fpDiff)
	}
}

func TestFindDuplicate(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_dedup_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := database.Open(filepath.Join(tempDir, "dedup.db"))
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Seed existing track
	track := &models.Track{
		ID:          "doc_5555",
		Title:       "Hotel California",
		Artist:      "Eagles",
		Album:       "Hotel California",
		DurationSec: 390,
		MimeType:    "audio/flac",
		FileSize:    25 * 1024 * 1024,
		FileName:    "eagles_hotel_california.flac",
		CreatedAt:   time.Now(),
		Fingerprint: MetadataFingerprint("Hotel California", "Eagles", "Hotel California", 390),
	}
	if err := db.SaveTrack(ctx, track); err != nil {
		t.Fatalf("Failed to save seed track: %v", err)
	}

	source := &models.TrackSource{
		TrackID:      "doc_5555",
		ChatID:       -100111222,
		MsgID:        100,
		TopicID:      1,
		FileUniqueID: "unique_5555",
		CreatedAt:    time.Now(),
	}
	if err := db.AddTrackSource(ctx, source); err != nil {
		t.Fatalf("Failed to add seed track source: %v", err)
	}

	// Case 1: Match by exact Telegram FileUniqueID
	dup, found, err := FindDuplicate(ctx, db, "unique_5555", "Different Title", "Different Artist", "", 0)
	if err != nil || !found || dup == nil || dup.ID != "doc_5555" {
		t.Errorf("Expected match by fileUniqueID, got dup=%+v (found: %t, err: %v)", dup, found, err)
	}

	// Case 2: Match by metadata fingerprint
	dup, found, err = FindDuplicate(ctx, db, "unique_other", "Hotel California", "Eagles", "Hotel California", 391)
	if err != nil || !found || dup == nil || dup.ID != "doc_5555" {
		t.Errorf("Expected match by fingerprint, got dup=%+v (found: %t, err: %v)", dup, found, err)
	}

	// Case 3: Match by fuzzy title + artist + duration tolerance (+-1s)
	dup, found, err = FindDuplicate(ctx, db, "unique_none", "hotel california", "eagles", "", 389)
	if err != nil || !found || dup == nil || dup.ID != "doc_5555" {
		t.Errorf("Expected match by fuzzy title/artist, got dup=%+v (found: %t, err: %v)", dup, found, err)
	}

	// Case 4: No match for completely different song
	dup, found, err = FindDuplicate(ctx, db, "unique_9999", "Yesterday", "The Beatles", "Help!", 125)
	if err != nil || found || dup != nil {
		t.Errorf("Expected no duplicate, got dup=%+v (found: %t)", dup, found)
	}
}
