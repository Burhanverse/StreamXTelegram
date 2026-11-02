package telegram

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

func TestBackupRestoreRoundTripAndCredentialHygiene(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_backup_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "original.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open source db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 1. Populate original database with tracks, sources, and sync state
	track := &models.Track{
		ID:          "doc_12345",
		Title:       "Imagine",
		Artist:      "John Lennon",
		Album:       "Imagine",
		DurationSec: 183,
		MimeType:    "audio/flac",
		FileSize:    24 * 1024 * 1024,
		FileName:    "01_imagine.flac",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveTrack(ctx, track); err != nil {
		t.Fatalf("Failed to save track: %v", err)
	}

	source := &models.TrackSource{
		TrackID:      "doc_12345",
		ChatID:       -1009876543210,
		MsgID:        300,
		TopicID:      42,
		FileUniqueID: "12345",
		CreatedAt:    time.Now(),
	}
	if err := db.AddTrackSource(ctx, source); err != nil {
		t.Fatalf("Failed to add track source: %v", err)
	}

	syncState := &models.SyncState{
		ChannelID: 9876543210,
		Pts:       1050,
		Date:      1700000000,
		Qts:       0,
		MaxMsgID:  300,
		UpdatedAt: time.Now(),
	}
	if err := db.SaveSyncState(ctx, syncState); err != nil {
		t.Fatalf("Failed to save sync state: %v", err)
	}

	// 2. Export VACUUM snapshot
	backupPath := filepath.Join(tempDir, "backup_snapshot.db")
	if err := db.ExportBackup(backupPath); err != nil {
		t.Fatalf("Failed to export backup: %v", err)
	}

	// 3. Verify SQLite header & Credential Hygiene
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("Failed to read backup file: %v", err)
	}

	if !bytes.HasPrefix(backupBytes, []byte("SQLite format 3\000")) {
		t.Errorf("Backup does not start with SQLite format 3 magic header")
	}

	// Strict verification: No bot tokens, secret keys, or api hashes in the DB file
	secretPatterns := []string{
		"bot_token",
		"api_hash",
		"session_token",
		"123456789:ABCdefGHIjklMNOpqrsTUVwxyz123456",
	}
	backupContent := string(backupBytes)
	for _, secret := range secretPatterns {
		if strings.Contains(backupContent, secret) {
			t.Errorf("Security risk: SQLite backup contains secret substring: %s", secret)
		}
	}

	// 4. Restore snapshot into a clean target database
	restoredDBPath := filepath.Join(tempDir, "restored.db")
	restoredDB, err := database.Open(restoredDBPath)
	if err != nil {
		t.Fatalf("Failed to open target db: %v", err)
	}
	defer restoredDB.Close()

	if err := restoredDB.ImportBackup(backupPath); err != nil {
		t.Fatalf("ImportBackup failed: %v", err)
	}

	// 5. Verify restored data
	restoredTrack, err := restoredDB.GetTrack(ctx, "doc_12345")
	if err != nil || restoredTrack == nil {
		t.Fatalf("Failed to query restored track: %v", err)
	}
	if restoredTrack.Title != "Imagine" || restoredTrack.Artist != "John Lennon" {
		t.Errorf("Restored track metadata mismatch: %+v", restoredTrack)
	}

	restoredSources, err := restoredDB.GetTrackSources(ctx, "doc_12345")
	if err != nil || len(restoredSources) != 1 {
		t.Fatalf("Expected 1 restored source, got %d", len(restoredSources))
	}
	if restoredSources[0].TopicID != 42 || restoredSources[0].MsgID != 300 {
		t.Errorf("Restored source mismatch: %+v", restoredSources[0])
	}

	restoredState, err := restoredDB.GetSyncState(ctx, 9876543210)
	if err != nil || restoredState == nil {
		t.Fatalf("Failed to query restored sync state: %v", err)
	}
	if restoredState.Pts != 1050 || restoredState.MaxMsgID != 300 {
		t.Errorf("Restored sync state mismatch: %+v", restoredState)
	}
}

func TestCorruptedBackupRejection(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_corrupt_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "active.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}
	defer db.Close()

	// Truncated/corrupted backup
	corruptFile := filepath.Join(tempDir, "corrupt.db")
	_ = os.WriteFile(corruptFile, []byte("Not an SQLite DB"), 0600)

	err = db.ImportBackup(corruptFile)
	if err == nil {
		t.Errorf("Expected ImportBackup to reject corrupted file, but succeeded")
	}
}
