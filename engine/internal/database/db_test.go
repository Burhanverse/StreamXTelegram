package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musix/engine/internal/models"
)

func TestDatabaseOperations(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "library.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 1. Track insertion & retrieval
	track := &models.Track{
		ID:          "test_track_1",
		Title:       "Sample Title",
		Artist:      "Sample Artist",
		Album:       "Sample Album",
		DurationSec: 180,
		MimeType:    "audio/flac",
		FileSize:    10485760,
		FileName:    "sample.flac",
	}

	if err := db.SaveTrack(ctx, track); err != nil {
		t.Fatalf("SaveTrack failed: %v", err)
	}

	retrieved, err := db.GetTrack(ctx, "test_track_1")
	if err != nil {
		t.Fatalf("GetTrack failed: %v", err)
	}
	if retrieved == nil || retrieved.Title != "Sample Title" || retrieved.DurationSec != 180 {
		t.Fatalf("GetTrack mismatch: %+v", retrieved)
	}

	// 2. Multi-source linking (one track -> multiple topics/messages)
	source1 := &models.TrackSource{
		TrackID:      "test_track_1",
		ChatID:       -100123456789,
		MsgID:        101,
		TopicID:      5,
		FileUniqueID: "unique_1",
	}
	source2 := &models.TrackSource{
		TrackID:      "test_track_1",
		ChatID:       -100123456789,
		MsgID:        202,
		TopicID:      12,
		FileUniqueID: "unique_1",
	}
	if err := db.AddTrackSource(ctx, source1); err != nil {
		t.Fatalf("AddTrackSource 1 failed: %v", err)
	}
	if err := db.AddTrackSource(ctx, source2); err != nil {
		t.Fatalf("AddTrackSource 2 failed: %v", err)
	}

	sources, err := db.GetTrackSources(ctx, "test_track_1")
	if err != nil {
		t.Fatalf("GetTrackSources failed: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(sources))
	}

	// 3. Worker location caching
	loc := &models.WorkerLocation{
		BotID:         "bot_2",
		MsgID:         101,
		AccessHash:    9988776655,
		FileReference: []byte{0xaa, 0xbb, 0xcc},
		DCID:          2,
	}
	if err := db.SaveWorkerLocation(ctx, loc); err != nil {
		t.Fatalf("SaveWorkerLocation failed: %v", err)
	}

	retrievedLoc, err := db.GetWorkerLocation(ctx, "bot_2", 101)
	if err != nil {
		t.Fatalf("GetWorkerLocation failed: %v", err)
	}
	if retrievedLoc == nil || retrievedLoc.AccessHash != 9988776655 {
		t.Fatalf("WorkerLocation mismatch: %+v", retrievedLoc)
	}

	// 4. Sync state persistence
	syncState := &models.SyncState{
		ChannelID: -100123456789,
		Pts:       450,
		Date:      int(time.Now().Unix()),
		MaxMsgID:  512,
	}
	if err := db.SaveSyncState(ctx, syncState); err != nil {
		t.Fatalf("SaveSyncState failed: %v", err)
	}

	retrievedState, err := db.GetSyncState(ctx, -100123456789)
	if err != nil {
		t.Fatalf("GetSyncState failed: %v", err)
	}
	if retrievedState.Pts != 450 || retrievedState.MaxMsgID != 512 {
		t.Fatalf("SyncState mismatch: %+v", retrievedState)
	}

	// 5. Backup export and restore
	backupPath := filepath.Join(tmpDir, "backup_test.db")
	if err := db.ExportBackup(backupPath); err != nil {
		t.Fatalf("ExportBackup failed: %v", err)
	}

	// Verify backup file exists and has size
	info, err := os.Stat(backupPath)
	if err != nil || info.Size() < 512 {
		t.Fatalf("backup file invalid or missing: %v", err)
	}

	// Create new fresh DB and restore backup into it
	restoredDBPath := filepath.Join(tmpDir, "restored.db")
	restoredDB, err := Open(restoredDBPath)
	if err != nil {
		t.Fatalf("failed to open restored db: %v", err)
	}
	defer restoredDB.Close()

	if err := restoredDB.ImportBackup(backupPath); err != nil {
		t.Fatalf("ImportBackup failed: %v", err)
	}

	// 6. Topic folders & playlist metadata
	testTopic := &models.Topic{
		ID:          42,
		ChatID:      -100123456789,
		Name:        "Rock Anthems",
		IconColor:   0x6FB9F0,
		IconEmojiID: 54321,
		IsFolder:    true,
	}
	if err := db.SaveTopic(ctx, testTopic); err != nil {
		t.Fatalf("SaveTopic failed: %v", err)
	}

	retrievedTopic, err := db.GetTopic(ctx, -100123456789, 42)
	if err != nil || retrievedTopic == nil {
		t.Fatalf("GetTopic failed: %v", err)
	}
	if retrievedTopic.Name != "Rock Anthems" || !retrievedTopic.IsFolder || retrievedTopic.IconColor != 0x6FB9F0 {
		t.Fatalf("Topic data mismatch: %+v", retrievedTopic)
	}

	// 7. Custom playlists & tracks
	testPl := &models.Playlist{
		ID:       "pl_rock_90s",
		ChatID:   -100123456789,
		TopicID:  42,
		Title:    "90s Grunge",
		Subtitle: "Best of Nirvana & Pearl Jam",
	}
	if err := db.CreatePlaylist(ctx, testPl); err != nil {
		t.Fatalf("CreatePlaylist failed: %v", err)
	}

	if err := db.AddTrackToPlaylist(ctx, "pl_rock_90s", "test_track_1", 1); err != nil {
		t.Fatalf("AddTrackToPlaylist failed: %v", err)
	}

	plTracks, err := db.GetPlaylistTracks(ctx, "pl_rock_90s", 0, 10)
	if err != nil || len(plTracks) != 1 {
		t.Fatalf("GetPlaylistTracks failed: len=%d err=%v", len(plTracks), err)
	}
	if plTracks[0].ID != "test_track_1" {
		t.Fatalf("Playlist track ID mismatch: %s", plTracks[0].ID)
	}

	// 8. Backup records indexing
	backupRecord := &models.BackupRecord{
		MsgID:      999,
		ChatID:     -100123456789,
		TopicID:    42,
		DocID:      11223344,
		AccessHash: 55667788,
		Caption:    "#musix_backup v1 timestamp=123456",
		CreatedAt:  time.Now(),
	}
	if err := db.SaveBackupRecord(ctx, backupRecord); err != nil {
		t.Fatalf("SaveBackupRecord failed: %v", err)
	}

	retrievedBackup, err := db.GetLatestBackup(ctx, -100123456789, 42)
	if err != nil || retrievedBackup == nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if retrievedBackup.MsgID != 999 || retrievedBackup.DocID != 11223344 {
		t.Fatalf("Backup record mismatch: %+v", retrievedBackup)
	}
}
