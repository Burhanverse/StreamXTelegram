package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musix/engine/internal/models"
)

func TestCatalogQueriesAndVirtualGrouping(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_catalog_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := Open(filepath.Join(tempDir, "catalog.db"))
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 1. Seed tracks across artists and albums
	tracks := []*models.Track{
		{
			ID:          "track_1",
			Title:       "Comfortably Numb",
			Artist:      "Pink Floyd",
			Album:       "The Wall",
			DurationSec: 382,
			MimeType:    "audio/flac",
			FileSize:    30 * 1024 * 1024,
			FileName:    "01_comfortably_numb.flac",
			CreatedAt:   time.Now().Add(-3 * time.Hour),
			CoverURL:     "https://example.com/the_wall.jpg",
			ThumbnailURL: "https://example.com/the_wall_thumb.jpg",
			Year:         1979,
		},
		{
			ID:           "track_2",
			Title:        "Time",
			Artist:       "Pink Floyd",
			Album:        "The Dark Side of the Moon",
			DurationSec:  425,
			MimeType:     "audio/flac",
			FileSize:     35 * 1024 * 1024,
			FileName:     "02_time.flac",
			CreatedAt:    time.Now().Add(-2 * time.Hour),
			CoverURL:     "https://example.com/dark_side.jpg",
			ThumbnailURL: "https://example.com/dark_side_thumb.jpg",
			Year:         1973,
		},
		{
			ID:           "track_3",
			Title:        "Bohemian Rhapsody",
			Artist:       "Queen",
			Album:        "A Night at the Opera",
			DurationSec:  354,
			MimeType:     "audio/mp3",
			FileSize:     12 * 1024 * 1024,
			FileName:     "03_bohemian.mp3",
			CreatedAt:    time.Now().Add(-1 * time.Hour),
			CoverURL:     "https://example.com/night_at_opera.jpg",
			ThumbnailURL: "https://example.com/night_at_opera_thumb.jpg",
			Year:         1975,
		},
	}

	for _, tr := range tracks {
		if err := db.SaveTrack(ctx, tr); err != nil {
			t.Fatalf("Failed to save track %s: %v", tr.ID, err)
		}
	}

	// 2. Link tracks to Telegram topics (playlists)
	sources := []*models.TrackSource{
		{
			TrackID:      "track_1",
			ChatID:       -100123456789,
			MsgID:        101,
			TopicID:      10, // Classic Rock Topic
			FileUniqueID: "unique_1",
			CreatedAt:    time.Now(),
		},
		{
			TrackID:      "track_2",
			ChatID:       -100123456789,
			MsgID:        102,
			TopicID:      10, // Classic Rock Topic
			FileUniqueID: "unique_2",
			CreatedAt:    time.Now(),
		},
		{
			TrackID:      "track_3",
			ChatID:       -100123456789,
			MsgID:        103,
			TopicID:      20, // Opera/Vocal Topic
			FileUniqueID: "unique_3",
			CreatedAt:    time.Now(),
		},
	}

	for _, src := range sources {
		if err := db.AddTrackSource(ctx, src); err != nil {
			t.Fatalf("Failed to add track source: %v", err)
		}
	}

	// 3. Test ListTracks (all tracks paginated)
	allTracks, total, err := db.ListTracks(ctx, 0, 10)
	if err != nil || total != 3 || len(allTracks) != 3 {
		t.Errorf("ListTracks failed: total=%d, len=%d, err=%v", total, len(allTracks), err)
	}

	// 4. Test ListTopics
	topics, err := db.ListTopics(ctx, -100123456789)
	if err != nil || len(topics) != 2 {
		t.Fatalf("ListTopics failed: len=%d, err=%v", len(topics), err)
	}
	// Topic 10 should have 2 tracks
	foundTopic10 := false
	for _, top := range topics {
		if top.ID == 10 {
			foundTopic10 = true
			if top.TrackCount != 2 {
				t.Errorf("Expected Topic 10 to have 2 tracks, got %d", top.TrackCount)
			}
		}
	}
	if !foundTopic10 {
		t.Errorf("Topic 10 not found in topics list")
	}

	// 5. Test ListTracksByTopic
	topic10Tracks, err := db.ListTracksByTopic(ctx, -100123456789, 10, 0, 10)
	if err != nil || len(topic10Tracks) != 2 {
		t.Errorf("ListTracksByTopic(10) failed: len=%d, err=%v", len(topic10Tracks), err)
	}

	// 6. Test ListArtists
	artists, artistCount, err := db.ListArtists(ctx, 0, 10)
	if err != nil || artistCount != 2 || len(artists) != 2 {
		t.Errorf("ListArtists failed: count=%d, len=%d, err=%v", artistCount, len(artists), err)
	}
	// Pink Floyd has 2 tracks, Queen has 1 track -> Pink Floyd should be first (ORDER BY track_count DESC)
	if artists[0].Name != "Pink Floyd" || artists[0].TrackCount != 2 {
		t.Errorf("Expected Pink Floyd first with 2 tracks, got %+v", artists[0])
	}

	// 7. Test ListAlbums
	albums, albumCount, err := db.ListAlbums(ctx, 0, 10)
	if err != nil || albumCount != 3 || len(albums) != 3 {
		t.Errorf("ListAlbums failed: count=%d, len=%d, err=%v", albumCount, len(albums), err)
	}

	// 8. Test SearchTracks
	searchRes, err := db.SearchTracks(ctx, "comfort", 10)
	if err != nil || len(searchRes) != 1 || searchRes[0].ID != "track_1" {
		t.Errorf("SearchTracks('comfort') failed: len=%d, err=%v", len(searchRes), err)
	}

	searchArtist, err := db.SearchTracks(ctx, "queen", 10)
	if err != nil || len(searchArtist) != 1 || searchArtist[0].ID != "track_3" {
		t.Errorf("SearchTracks('queen') failed: len=%d, err=%v", len(searchArtist), err)
	}

	// 9. Test GetRandomTracks
	randomTracks, err := db.GetRandomTracks(ctx, 2)
	if err != nil || len(randomTracks) != 2 {
		t.Errorf("GetRandomTracks failed: len=%d, err=%v", len(randomTracks), err)
	}

	// 10. Test UpdateTrackEnrichment
	err = db.UpdateTrackEnrichment(ctx, "track_1", "https://new.cover.com/hd.jpg", "", "Hello, is there anybody in there?", "[00:10.00] Hello", 1979)
	if err != nil {
		t.Fatalf("UpdateTrackEnrichment failed: %v", err)
	}
	updated, err := db.GetTrack(ctx, "track_1")
	if err != nil || updated.CoverURL != "https://new.cover.com/hd.jpg" || updated.Lyrics == "" {
		t.Errorf("Track enrichment not persisted: %+v", updated)
	}
}
