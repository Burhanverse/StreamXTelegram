package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

func setupAPITestServer(t *testing.T) (*Server, *database.DB) {
	tempDir, err := os.MkdirTemp("", "musix_api_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	db, err := database.Open(filepath.Join(tempDir, "api.db"))
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mockClient := telegram.NewMockClient()
	_ = mockClient.Start(context.Background())
	t.Cleanup(func() { mockClient.Stop() })

	ctx := context.Background()
	chatID := int64(-100998877)

	// Seed tracks
	tracks := []*models.Track{
		{
			ID:           "doc_101",
			Title:        "Time",
			Artist:       "Pink Floyd",
			Album:        "The Dark Side of the Moon",
			DurationSec:  425,
			MimeType:     "audio/flac",
			FileSize:     30 * 1024 * 1024,
			FileName:     "time.flac",
			CreatedAt:    time.Now().Add(-2 * time.Hour),
			CoverURL:     "https://example.com/dark_side.jpg",
			ThumbnailURL: "https://example.com/dark_side_thumb.jpg",
			Lyrics:       "Ticking away the moments that make up a dull day",
			SyncedLyrics: "[00:10.00] Ticking away the moments",
			Year:         1973,
		},
		{
			ID:          "doc_102",
			Title:       "Money",
			Artist:      "Pink Floyd",
			Album:       "The Dark Side of the Moon",
			DurationSec: 382,
			MimeType:    "audio/flac",
			FileSize:    28 * 1024 * 1024,
			FileName:    "money.flac",
			CreatedAt:   time.Now().Add(-1 * time.Hour),
			CoverURL:    "https://example.com/dark_side.jpg",
			Year:        1973,
		},
		{
			ID:          "doc_103",
			Title:       "Killer Queen",
			Artist:      "Queen",
			Album:       "Sheer Heart Attack",
			DurationSec: 180,
			MimeType:    "audio/mp3",
			FileSize:    7 * 1024 * 1024,
			FileName:    "killer_queen.mp3",
			CreatedAt:   time.Now(),
			Year:        1974,
		},
	}

	for _, tr := range tracks {
		if err := db.SaveTrack(ctx, tr); err != nil {
			t.Fatalf("Failed to save track: %v", err)
		}
		mockClient.RegisterTrack(tr)
	}

	// Seed topics / track sources
	_ = db.SaveTopic(ctx, &models.Topic{
		ID:        1,
		ChatID:    chatID,
		Name:      "Progressive Rock",
		UpdatedAt: time.Now(),
	})

	_ = db.AddTrackSource(ctx, &models.TrackSource{
		TrackID:      "doc_101",
		ChatID:       chatID,
		MsgID:        501,
		TopicID:      1,
		FileUniqueID: "unique_101",
		CreatedAt:    time.Now(),
	})
	_ = db.AddTrackSource(ctx, &models.TrackSource{
		TrackID:      "doc_102",
		ChatID:       chatID,
		MsgID:        502,
		TopicID:      1,
		FileUniqueID: "unique_102",
		CreatedAt:    time.Now(),
	})

	srv, err := New(Config{
		Token:  "secret-test-token",
		Port:   0,
		Client: mockClient,
		DB:     db,
		ChatID: chatID,
	})
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })

	return srv, db
}

func doAPIRequest(t *testing.T, srv *Server, path string) (int, map[string]interface{}) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", srv.Port(), path)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Request %s failed: %v", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var payload map[string]interface{}
	_ = json.Unmarshal(body, &payload)

	return resp.StatusCode, payload
}

func TestRESTAPIRoutes(t *testing.T) {
	srv, _ := setupAPITestServer(t)

	// 1. GET /api/topics
	status, data := doAPIRequest(t, srv, "/api/topics")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/topics failed: status=%d, data=%+v", status, data)
	}
	items, ok := data["items"].([]interface{})
	if !ok || len(items) != 1 {
		t.Errorf("Expected 1 topic, got %+v", items)
	}

	// 2. GET /api/topics/1
	status, data = doAPIRequest(t, srv, "/api/topics/1")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/topics/1 failed: status=%d", status)
	}
	topicTracks := data["items"].([]interface{})
	if len(topicTracks) != 2 {
		t.Errorf("Expected 2 tracks in topic 1, got %d", len(topicTracks))
	}

	// 3. GET /api/artists
	status, data = doAPIRequest(t, srv, "/api/artists")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/artists failed: status=%d", status)
	}
	artists := data["items"].([]interface{})
	if len(artists) != 2 {
		t.Errorf("Expected 2 artists, got %d", len(artists))
	}

	// 4. GET /api/artists/Pink Floyd
	status, data = doAPIRequest(t, srv, "/api/artists/Pink%20Floyd")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/artists/Pink Floyd failed: status=%d", status)
	}
	artistTracks := data["tracks"].([]interface{})
	if len(artistTracks) != 2 {
		t.Errorf("Expected 2 tracks for Pink Floyd, got %d", len(artistTracks))
	}

	// 5. GET /api/albums
	status, data = doAPIRequest(t, srv, "/api/albums")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/albums failed: status=%d", status)
	}
	albums := data["items"].([]interface{})
	if len(albums) != 2 {
		t.Errorf("Expected 2 albums, got %d", len(albums))
	}

	// 6. GET /api/albums/The Dark Side of the Moon
	status, data = doAPIRequest(t, srv, "/api/albums/The%20Dark%20Side%20of%20the%20Moon")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/albums/... failed: status=%d", status)
	}
	albumTracks := data["tracks"].([]interface{})
	if len(albumTracks) != 2 {
		t.Errorf("Expected 2 tracks in Dark Side album, got %d", len(albumTracks))
	}

	// 7. GET /api/tracks
	status, data = doAPIRequest(t, srv, "/api/tracks?limit=2")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/tracks failed: status=%d", status)
	}
	tracksList := data["items"].([]interface{})
	if len(tracksList) != 2 {
		t.Errorf("Expected 2 paginated tracks, got %d", len(tracksList))
	}

	// 8. GET /api/search?q=killer
	status, data = doAPIRequest(t, srv, "/api/search?q=killer")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/search failed: status=%d", status)
	}
	searchResults := data["items"].([]interface{})
	if len(searchResults) != 1 {
		t.Errorf("Expected 1 search result for 'killer', got %d", len(searchResults))
	}

	// 9. GET /api/daily_mix
	status, data = doAPIRequest(t, srv, "/api/daily_mix?limit=2")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/api/daily_mix failed: status=%d", status)
	}
	dailyItems := data["items"].([]interface{})
	if len(dailyItems) != 2 {
		t.Errorf("Expected 2 daily mix items, got %d", len(dailyItems))
	}

	// 10. GET /lyrics/doc_101
	status, data = doAPIRequest(t, srv, "/lyrics/doc_101")
	if status != http.StatusOK || data["ok"] != true {
		t.Fatalf("/lyrics/doc_101 failed: status=%d", status)
	}
	if data["plain_lyrics"] != "Ticking away the moments that make up a dull day" {
		t.Errorf("Lyrics mismatch: %+v", data)
	}

	// 11. GET /artwork/doc_101 (Redirects to CoverURL)
	artURL := fmt.Sprintf("http://127.0.0.1:%d/artwork/doc_101?token=%s", srv.Port(), srv.Token())
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirect
		},
	}
	resp, err := noRedirectClient.Get(artURL)
	if err != nil {
		t.Fatalf("Artwork GET failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("Expected 302 redirect for artwork, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://example.com/dark_side.jpg" {
		t.Errorf("Expected Location header https://example.com/dark_side.jpg, got %q", loc)
	}
}

func TestParallelPrefetchBuffer(t *testing.T) {
	mockClient := telegram.NewMockClient()
	_ = mockClient.Start(context.Background())
	defer mockClient.Stop()

	// Create 3 MB test audio payload
	size := 3 * 1024 * 1024
	testData := make([]byte, size)
	for i := range testData {
		testData[i] = byte(i % 255)
	}

	track := &models.Track{
		ID:          "stream_prefetch_test",
		Title:       "Prefetch Demo",
		Artist:      "Go Audio Engine",
		DurationSec: 300,
		MimeType:    "audio/flac",
		FileSize:    int64(size),
		FileName:    "test.flac",
		AccessHash:  999,
	}
	mockClient.RegisterTrackWithPayload(track, testData)

	srv, err := New(Config{
		Token:  "test-prefetch-token",
		Port:   0,
		Client: mockClient,
	})
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	srv.Start()
	defer srv.Close()

	streamURL := fmt.Sprintf("http://127.0.0.1:%d/stream/%s?token=%s", srv.Port(), track.ID, srv.Token())

	// 1. Request Chunk 0 (0 to 524287: 512 KB)
	req1, _ := http.NewRequest(http.MethodGet, streamURL, nil)
	req1.Header.Set("Range", "bytes=0-524287")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("Chunk 0 failed: %v", err)
	}
	chunk0, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	if len(chunk0) != 512*1024 {
		t.Fatalf("Chunk 0 length mismatch: got %d, want 524288", len(chunk0))
	}

	// 2. Allow background goroutine to prefetch Chunk 1 (524288 to 1048575)
	time.Sleep(60 * time.Millisecond)

	// 3. Request Chunk 1 and measure latency
	t0 := time.Now()
	req2, _ := http.NewRequest(http.MethodGet, streamURL, nil)
	req2.Header.Set("Range", "bytes=524288-1048575")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Chunk 1 failed: %v", err)
	}
	var firstByte [1]byte
	_, _ = io.ReadFull(resp2.Body, firstByte[:])
	prefetechedLatency := time.Since(t0)
	chunk1Rest, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	totalChunk1Len := 1 + len(chunk1Rest)
	if totalChunk1Len != 512*1024 {
		t.Errorf("Chunk 1 length mismatch: got %d, want 524288", totalChunk1Len)
	}

	t.Logf("Prefetched Chunk 1 TTFB Latency: %v", prefetechedLatency)

	// In-memory loopback prefetch latency should be sub-millisecond or few milliseconds
	if prefetechedLatency > 50*time.Millisecond {
		t.Errorf("Prefetched chunk latency too high: %v", prefetechedLatency)
	}
}
