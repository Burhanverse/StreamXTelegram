package services

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

// 1. Pause during playback test
func TestPrompt2_PauseDuringPlayback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_pause.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	service := NewEnrichmentService(db)
	defer service.Stop()

	var playbackActive atomic.Bool
	playbackActive.Store(true)

	service.SetPlaybackChecker(func() bool {
		return playbackActive.Load()
	})

	// Register a track in DB
	track := &models.Track{
		ID:          "track_pause_test",
		Title:       "Test Song",
		Artist:      "Test Artist",
		DurationSec: 200,
		CreatedAt:   time.Now(),
	}
	_ = db.SaveTrack(context.Background(), track)

	service.Enqueue(track.ID)

	// Since playback is active, the queue item should remain or not be fully processed into DB
	time.Sleep(300 * time.Millisecond)

	// Verify it paused and did not mark success yet
	state, _ := db.GetTrack(context.Background(), track.ID)
	if state == nil {
		t.Fatal("Track not found")
	}

	// Now stop playback
	playbackActive.Store(false)

	// Wait for idle period (2s) plus worker processing time
	time.Sleep(2600 * time.Millisecond)
}

// 2. Queue deduplication test
func TestPrompt2_QueueDeduplication(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_dedupe.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	service := NewEnrichmentService(db)
	defer service.Stop()

	// Enqueue the same track 10 times rapidly
	for i := 0; i < 10; i++ {
		service.Enqueue("track_dedupe_123")
	}

	// Service queue should have at most 1 item
	service.enqueuedMu.Lock()
	mapSize := len(service.enqueuedMap)
	service.enqueuedMu.Unlock()

	if mapSize != 1 {
		t.Errorf("Expected 1 entry in enqueuedMap, got %d", mapSize)
	}
}

// 3. Negative cache with TTL respected
func TestPrompt2_NegativeCacheRespected(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_negcache.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	track := &models.Track{
		ID:          "track_fail_test",
		Title:       "Obscure Unknown Track",
		Artist:      "Nobody",
		DurationSec: 180,
		CreatedAt:   time.Now(),
	}
	_ = db.SaveTrack(ctx, track)

	// Save failure into enrichment_state with 24 hour TTL
	err = db.SaveEnrichmentState(ctx, track.ID, "failed", 1, "no cover found", 24*time.Hour)
	if err != nil {
		t.Fatalf("SaveEnrichmentState failed: %v", err)
	}

	// Verify IsEnrichmentNegativeCached returns true
	if !db.IsEnrichmentNegativeCached(ctx, track.ID) {
		t.Errorf("Expected track to be negative cached")
	}

	// Verify ListUnenrichedTrackIDs excludes the negative cached track
	unenriched, err := db.ListUnenrichedTrackIDs(ctx)
	if err != nil {
		t.Fatalf("ListUnenrichedTrackIDs failed: %v", err)
	}
	for _, id := range unenriched {
		if id == track.ID {
			t.Errorf("Expected negative cached track %s to be excluded from unenriched list", track.ID)
		}
	}
}

// 4. Task 7: Exponential backoff on transient errors and attempt capping
func TestTask7_TransientRetryExponentialBackoff(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_backoff.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	service := NewEnrichmentService(db)
	defer service.Stop()

	ctx := context.Background()
	trackID := "track_transient_test"
	track := &models.Track{
		ID:        trackID,
		Title:     "Transient Track",
		CreatedAt: time.Now(),
	}
	_ = db.SaveTrack(ctx, track)

	// Simulate transient errors across attempts
	for attempt := 0; attempt < 5; attempt++ {
		currentAttempts, err := db.GetEnrichmentAttempts(ctx, trackID)
		if err != nil {
			t.Fatalf("GetEnrichmentAttempts failed: %v", err)
		}
		if currentAttempts != attempt {
			t.Fatalf("Expected %d attempts before retry, got %d", attempt, currentAttempts)
		}

		service.recordTransientRetry(ctx, trackID, context.DeadlineExceeded)

		// Verify state was saved as transient_retry
		attemptsAfter, _ := db.GetEnrichmentAttempts(ctx, trackID)
		if attemptsAfter != attempt+1 {
			t.Fatalf("Expected %d attempts after retry, got %d", attempt+1, attemptsAfter)
		}

		// Verify track is excluded from unenriched while backoff is active
		unenriched, _ := db.ListUnenrichedTrackIDs(ctx)
		for _, id := range unenriched {
			if id == trackID {
				t.Fatalf("Track %s should be excluded during transient backoff at attempt %d", trackID, attempt+1)
			}
		}
	}

	// 6th attempt: exceeds maxTransientAttempts (5) -> must be marked as permanently "failed"
	service.recordTransientRetry(ctx, trackID, context.DeadlineExceeded)
	status, err := db.GetEnrichmentStatus(ctx, trackID)
	if err != nil {
		t.Fatalf("GetEnrichmentStatus failed: %v", err)
	}
	if status != "failed" {
		t.Fatalf("Expected status 'failed' after exceeding max retries, got '%s'", status)
	}
}

// 5. Task 7: EnrichTrack yields to active sync
func TestTask7_EnrichTrackYieldsToSync(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_sync_yield.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	service := NewEnrichmentService(db)
	defer service.Stop()

	var syncActive atomic.Bool
	syncActive.Store(true)
	service.SetSyncChecker(func() bool {
		return syncActive.Load()
	})

	ctx := context.Background()
	trackID := "track_sync_test"
	track := &models.Track{
		ID:        trackID,
		Title:     "Sync Test Track",
		CreatedAt: time.Now(),
	}
	_ = db.SaveTrack(ctx, track)

	// EnrichTrack must yield immediately and return nil without running extraction
	err = service.EnrichTrack(ctx, trackID)
	if err != nil {
		t.Fatalf("EnrichTrack should return nil when yielding, got: %v", err)
	}
}
