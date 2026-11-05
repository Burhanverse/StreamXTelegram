package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

func setupTestStreamXServer(t *testing.T) (*Server, *database.DB, string) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := database.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open failed: %v", err)
	}

	ctx := context.Background()
	chatID := int64(-100123456789)

	// Insert test tracks
	tr1 := &models.Track{
		ID:          "streamx_track_1",
		Title:       "Bohemian Rhapsody",
		Artist:      "Queen",
		Album:       "A Night at the Opera",
		DurationSec: 354,
		MimeType:    "audio/mp3",
		FileSize:    5000000,
		FileName:    "queen_bohemian.mp3",
		CoverURL:    "https://example.com/queen.jpg",
	}
	tr2 := &models.Track{
		ID:          "streamx_track_2",
		Title:       "Stairway to Heaven",
		Artist:      "Led Zeppelin",
		Album:       "Led Zeppelin IV",
		DurationSec: 482,
		MimeType:    "audio/flac",
		FileSize:    25000000,
		FileName:    "led_stairway.flac",
	}

	_ = db.SaveTrack(ctx, tr1)
	_ = db.SaveTrack(ctx, tr2)
	_ = db.AddTrackSource(ctx, &models.TrackSource{TrackID: tr1.ID, ChatID: chatID, TopicID: 100, MsgID: 10})
	_ = db.AddTrackSource(ctx, &models.TrackSource{TrackID: tr2.ID, ChatID: chatID, TopicID: 200, MsgID: 20})

	mockClient := telegram.NewMockClient()
	mockClient.RegisterTrackWithPayload(tr1, make([]byte, 1024))
	mockClient.RegisterTrackWithPayload(tr2, make([]byte, 1024))

	srv, err := New(Config{
		Token:  "test-streamx-token",
		Port:   0,
		Client: mockClient,
		DB:     db,
		ChatID: chatID,
	})
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}

	return srv, db, "test-streamx-token"
}

func TestStreamXHealth(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/health?token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["ok"] != true {
		t.Errorf("expected ok: true, got %v", resp["ok"])
	}
	caps, ok := resp["capabilities"].([]interface{})
	if !ok || len(caps) == 0 {
		t.Errorf("expected capabilities array, got %v", resp["capabilities"])
	}
}

func TestStreamXAccount(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/account?token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["id"] == "" || resp["name"] == "" {
		t.Errorf("expected account fields, got %v", resp)
	}
}

func TestStreamXHome(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/home?token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	sections, ok := resp["sections"].([]interface{})
	if !ok || len(sections) == 0 {
		t.Fatalf("expected non-empty sections in home, got %v", resp["sections"])
	}
}

func TestStreamXBrowse(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/browse?token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	sections, ok := resp["sections"].([]interface{})
	if !ok || len(sections) == 0 {
		t.Fatalf("expected non-empty sections in browse, got %v", resp["sections"])
	}
}

func TestStreamXLibrary(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/library?token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	sections, ok := resp["sections"].([]interface{})
	if !ok || len(sections) == 0 {
		t.Fatalf("expected non-empty sections in library, got %v", resp["sections"])
	}
}

func TestStreamXSearch(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/search?query=Bohemian&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	items, ok := resp["items"].([]interface{})
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty search items, got %v", resp["items"])
	}

	item0 := items[0].(map[string]interface{})
	if item0["type"] != "track" || item0["id"] != "streamx_track_1" {
		t.Errorf("expected track 1, got %v", item0)
	}
}

func TestStreamXDetail(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	// 1. Playlist Detail
	req := httptest.NewRequest("GET", "/api/streamx/detail?kind=playlist&id=100&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for playlist detail, got %d: %s", rec.Code, rec.Body.String())
	}

	var plResp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &plResp)
	if plResp["kind"] != "playlist" {
		t.Errorf("expected kind playlist, got %v", plResp["kind"])
	}

	// 2. Album Detail
	req2 := httptest.NewRequest("GET", "/api/streamx/detail?kind=album&id=Led+Zeppelin+IV&token="+token, nil)
	rec2 := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for album detail, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. Artist Detail
	req3 := httptest.NewRequest("GET", "/api/streamx/detail?kind=artist&id=Queen&token="+token, nil)
	rec3 := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 for artist detail, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestStreamXStreamAndSaved(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	// Stream URL
	req := httptest.NewRequest("GET", "/api/streamx/stream?id=streamx_track_1&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var streamResp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &streamResp)
	url, _ := streamResp["url"].(string)
	if url == "" {
		t.Errorf("expected stream url, got %v", streamResp)
	}

	// Saved
	req2 := httptest.NewRequest("GET", "/api/streamx/saved?kind=track&id=streamx_track_1&token="+token, nil)
	rec2 := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	// Save
	req3 := httptest.NewRequest("GET", "/api/streamx/save?kind=track&id=streamx_track_1&saved=true&token="+token, nil)
	rec3 := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec3.Code)
	}
}

func TestStreamXLyrics(t *testing.T) {
	srv, db, token := setupTestStreamXServer(t)
	defer srv.Close()

	// Seed lyrics
	ctx := context.Background()
	_ = db.SaveTrack(ctx, &models.Track{
		ID:           "lyrics_track_1",
		Title:        "Bohemian Rhapsody",
		Artist:       "Queen",
		Lyrics:       "Is this the real life? Is this just fantasy?",
		SyncedLyrics: "[00:01.00] Is this the real life?\n[00:04.50] Is this just fantasy?",
	})

	// 1. Fetch existing lyrics
	req := httptest.NewRequest("GET", "/api/streamx/lyrics?track_id=lyrics_track_1&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["plain"] != "Is this the real life? Is this just fantasy?" {
		t.Errorf("expected plain lyrics, got %v", resp["plain"])
	}
	if !strings.Contains(resp["synced"].(string), "[00:01.00]") {
		t.Errorf("expected synced lyrics, got %v", resp["synced"])
	}

	// 2. Fetch missing lyrics
	req2 := httptest.NewRequest("GET", "/api/streamx/lyrics?track_id=missing_track&token="+token, nil)
	rec2 := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	var resp2 map[string]interface{}
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["plain"] != nil || resp2["synced"] != nil {
		t.Errorf("expected empty lyrics for missing track, got %v", resp2)
	}
}

func TestStreamXPlaylists(t *testing.T) {
	srv, _, token := setupTestStreamXServer(t)
	defer srv.Close()

	req := httptest.NewRequest("GET", "/api/streamx/playlists?offset=0&limit=10&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	items, ok := resp["items"].([]interface{})
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty playlists items, got %v", resp)
	}
}

func TestStreamXResponseBudgetWithLargeFixture(t *testing.T) {
	srv, db, token := setupTestStreamXServer(t)
	defer srv.Close()

	ctx := context.Background()
	const totalTracks = 10000
	const batchSize = 1000

	// Insert 10,000 tracks in batches
	for b := 0; b < totalTracks/batchSize; b++ {
		tracks := make([]*models.Track, batchSize)
		for i := 0; i < batchSize; i++ {
			idx := b*batchSize + i
			tracks[i] = &models.Track{
				ID:          fmt.Sprintf("fixture_track_%05d", idx),
				Title:       fmt.Sprintf("Fixture Song Title #%05d With Detailed Track Information", idx),
				Artist:      fmt.Sprintf("Fixture Artist #%03d", idx%100),
				Album:       fmt.Sprintf("Fixture Album #%03d", idx%50),
				DurationSec: 180 + (idx % 120),
				FileSize:    5 * 1024 * 1024,
				MimeType:    "audio/mpeg",
			}
		}
		if err := db.SaveTracksBatch(ctx, tracks); err != nil {
			t.Fatalf("failed to insert batch %d: %v", b, err)
		}
	}

	// 1. Assert all paginated endpoints stay strictly under 200 KB
	endpoints := []string{
		"/api/streamx/home?token=" + token,
		"/api/streamx/browse?token=" + token,
		"/api/streamx/library?token=" + token,
		"/api/streamx/search?query=Fixture&limit=50&token=" + token,
		"/api/streamx/detail?kind=collection&id=all&offset=0&limit=50&token=" + token,
		"/api/streamx/playlists?offset=0&limit=50&token=" + token,
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest("GET", ep, nil)
		rec := httptest.NewRecorder()
		srv.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned status %d", ep, rec.Code)
		}
		bodyLen := rec.Body.Len()
		if bodyLen > 200*1024 {
			t.Fatalf("%s response size %d exceeds 200 KB budget", ep, bodyLen)
		}
	}

	// 2. Paginate collection with 10,000 tracks and assert every single item is returned exactly once
	seenIDs := make(map[string]bool)
	offset := 0
	pages := 0

	for {
		pages++
		if pages > 300 {
			t.Fatalf("exceeded 300 pages during pagination")
		}

		url := fmt.Sprintf("/api/streamx/detail?kind=collection&id=all&offset=%d&limit=50&token=%s", offset, token)
		req := httptest.NewRequest("GET", url, nil)
		rec := httptest.NewRecorder()
		srv.httpSrv.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("page %d returned %d", pages, rec.Code)
		}
		if rec.Body.Len() > 200*1024 {
			t.Fatalf("page %d size %d exceeds 200 KB cap", pages, rec.Body.Len())
		}

		var pageResp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &pageResp); err != nil {
			t.Fatalf("page %d invalid json: %v", pages, err)
		}

		items, ok := pageResp["items"].([]interface{})
		if !ok || len(items) == 0 {
			break
		}

		for _, it := range items {
			itemMap := it.(map[string]interface{})
			id := itemMap["id"].(string)
			if seenIDs[id] {
				t.Fatalf("duplicate track id encountered during pagination: %s", id)
			}
			seenIDs[id] = true
		}

		nextOffsetVal := pageResp["next_offset"]
		if nextOffsetVal == nil {
			break
		}
		nextOffset := int(nextOffsetVal.(float64))
		if nextOffset <= offset {
			break
		}
		offset = nextOffset
	}

	// Assert pagination reached all 10,000+ tracks
	if len(seenIDs) < totalTracks {
		t.Errorf("pagination returned %d unique tracks, want >= %d", len(seenIDs), totalTracks)
	}
}

func TestStreamXTopicPlaylistsAndFolders(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := database.Open(filepath.Join(tmpDir, "library.db"))
	if err != nil {
		t.Fatalf("db open error: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	chatID := int64(-100123456789)

	// Seed 2 topics
	_ = db.SaveTopic(ctx, &models.Topic{ID: 10, ChatID: chatID, Name: "EDM Beats", IsFolder: true})
	_ = db.SaveTopic(ctx, &models.Topic{ID: 20, ChatID: chatID, Name: "Acoustic Relax", IsFolder: true})

	// Seed 2 tracks linked to topic 10
	t1 := &models.Track{ID: "tr_edm_1", Title: "Titanium", Artist: "David Guetta", DurationSec: 240, MimeType: "audio/mp3", FileSize: 5000000, FileName: "titanium.mp3"}
	t2 := &models.Track{ID: "tr_edm_2", Title: "Levels", Artist: "Avicii", DurationSec: 200, MimeType: "audio/mp3", FileSize: 4500000, FileName: "levels.mp3"}
	_ = db.SaveTrack(ctx, t1)
	_ = db.SaveTrack(ctx, t2)
	_ = db.AddTrackSource(ctx, &models.TrackSource{TrackID: t1.ID, ChatID: chatID, MsgID: 101, TopicID: 10, FileUniqueID: "u1"})
	_ = db.AddTrackSource(ctx, &models.TrackSource{TrackID: t2.ID, ChatID: chatID, MsgID: 102, TopicID: 10, FileUniqueID: "u2"})

	srv, err := New(Config{
		Token:  "test_sec_token",
		Port:   0,
		DB:     db,
		ChatID: chatID,
	})
	if err != nil {
		t.Fatalf("New server error: %v", err)
	}
	defer srv.Close()
	token := srv.Token()

	// 1. Root playlists query (folder == "")
	req := httptest.NewRequest("GET", "/api/streamx/playlists?offset=0&limit=10&token="+token, nil)
	rec := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlists root returned %d", rec.Code)
	}

	var rootResp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &rootResp)
	folders, ok := rootResp["folders"].([]interface{})
	if !ok || len(folders) < 2 {
		t.Fatalf("expected at least 2 topic folders in root response, got %v", rootResp["folders"])
	}

	// 2. Query specific topic folder (folder == "10")
	reqFolder := httptest.NewRequest("GET", "/api/streamx/playlists?offset=0&limit=10&folder=10&token="+token, nil)
	recFolder := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recFolder, reqFolder)
	if recFolder.Code != http.StatusOK {
		t.Fatalf("playlists folder 10 returned %d", recFolder.Code)
	}

	var folderResp map[string]interface{}
	_ = json.Unmarshal(recFolder.Body.Bytes(), &folderResp)
	folderItems, _ := folderResp["items"].([]interface{})
	if len(folderItems) != 1 {
		t.Fatalf("expected 1 playlist inside topic folder 10, got %d", len(folderItems))
	}

	// 3. Query collection=library (must return playlists with type=playlist for libraryPlaylists())
	reqLib := httptest.NewRequest("GET", "/api/streamx/detail?kind=collection&id=library&token="+token, nil)
	recLib := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recLib, reqLib)
	if recLib.Code != http.StatusOK {
		t.Fatalf("detail library returned %d", recLib.Code)
	}

	var libResp map[string]interface{}
	_ = json.Unmarshal(recLib.Body.Bytes(), &libResp)
	libItems, _ := libResp["items"].([]interface{})
	if len(libItems) == 0 {
		t.Fatalf("expected non-empty items in library playlists")
	}
	firstPl := libItems[0].(map[string]interface{})
	if firstPl["type"] != "playlist" {
		t.Fatalf("expected item type 'playlist', got '%v'", firstPl["type"])
	}

	// 4. Playlist detail query (kind=playlist, id=10)
	reqDetail := httptest.NewRequest("GET", "/api/streamx/detail?kind=playlist&id=10&token="+token, nil)
	recDetail := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recDetail, reqDetail)
	if recDetail.Code != http.StatusOK {
		t.Fatalf("playlist detail returned %d", recDetail.Code)
	}

	var detailResp map[string]interface{}
	_ = json.Unmarshal(recDetail.Body.Bytes(), &detailResp)
	if detailResp["title"] != "EDM Beats" {
		t.Errorf("expected topic title 'EDM Beats', got '%v'", detailResp["title"])
	}
	if detailResp["subtitle"] != "2 tracks" {
		t.Errorf("expected topic subtitle '2 tracks', got '%v'", detailResp["subtitle"])
	}
	detailTracks, _ := detailResp["items"].([]interface{})
	if len(detailTracks) != 2 {
		t.Errorf("expected 2 tracks in topic 10 playlist, got %d", len(detailTracks))
	}

	// 5. Create playlist, add track, remove track
	reqCreate := httptest.NewRequest("GET", "/api/streamx/create_playlist?title=Party+Hits&token="+token, nil)
	recCreate := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusOK {
		t.Fatalf("create_playlist returned %d", recCreate.Code)
	}
	var createResp map[string]interface{}
	_ = json.Unmarshal(recCreate.Body.Bytes(), &createResp)
	plID := createResp["id"].(string)

	reqAdd := httptest.NewRequest("GET", fmt.Sprintf("/api/streamx/add_to_playlist?id=%s&tracks=[\"tr_edm_1\"]&token=%s", plID, token), nil)
	recAdd := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recAdd, reqAdd)
	if recAdd.Code != http.StatusOK {
		t.Fatalf("add_to_playlist returned %d", recAdd.Code)
	}

	reqRemove := httptest.NewRequest("GET", fmt.Sprintf("/api/streamx/remove_from_playlist?id=%s&track=tr_edm_1&token=%s", plID, token), nil)
	recRemove := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recRemove, reqRemove)
	if recRemove.Code != http.StatusOK {
		t.Fatalf("remove_from_playlist returned %d", recRemove.Code)
	}
}

func TestStreamXArtworkResolution(t *testing.T) {
	srv, db, token := setupTestStreamXServer(t)
	defer srv.Close()

	ctx := context.Background()

	// 1. tr1 has https://example.com/queen.jpg
	tr1, _ := db.GetTrack(ctx, "streamx_track_1")
	if art := srv.trackArtworkURL(tr1); art != "https://example.com/queen.jpg" {
		t.Errorf("expected tr1 artwork 'https://example.com/queen.jpg', got '%s'", art)
	}

	// 2. formatThumbnailURL should reject non-https URLs
	if art := srv.formatThumbnailURL("http://127.0.0.1:8080/artwork/1"); art != "" {
		t.Errorf("expected http:// URL to be rejected, got '%s'", art)
	}
	if art := srv.formatThumbnailURL("https://is1-ssl.mzstatic.com/art.jpg"); art != "https://is1-ssl.mzstatic.com/art.jpg" {
		t.Errorf("expected https:// URL to be preserved, got '%s'", art)
	}

	// 3. topicArtworkURL should fall back to first track's HTTPS artwork
	// Topic 100 has tr1 which has https://example.com/queen.jpg
	if art := srv.topicArtworkURL(ctx, 100, ""); art != "https://example.com/queen.jpg" {
		t.Errorf("expected topic 100 to fall back to 'https://example.com/queen.jpg', got '%s'", art)
	}

	// 4. albumArtworkURL should fall back to first track's HTTPS artwork
	if art := srv.albumArtworkURL(ctx, "A Night at the Opera", ""); art != "https://example.com/queen.jpg" {
		t.Errorf("expected album to fall back to 'https://example.com/queen.jpg', got '%s'", art)
	}

	// 5. artistArtworkURL should NOT reuse the first track's cover as an artist photo (Prompt 4 requirement)
	if art := srv.artistArtworkURL(ctx, "Queen", ""); art != "" {
		t.Errorf("expected artist art without verified photo to be empty, got '%s'", art)
	}
	if art := srv.artistArtworkURL(ctx, "Queen", "https://example.com/queen_artist.jpg"); art != "https://example.com/queen_artist.jpg" {
		t.Errorf("expected explicit artist art to be preserved, got '%s'", art)
	}

	// 6. Home endpoint returns topic playlist with valid HTTPS thumbnail
	reqHome := httptest.NewRequest("GET", "/api/streamx/home?token="+token, nil)
	recHome := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(recHome, reqHome)
	if recHome.Code != http.StatusOK {
		t.Fatalf("home returned %d", recHome.Code)
	}
	var homeResp map[string]interface{}
	_ = json.Unmarshal(recHome.Body.Bytes(), &homeResp)
	sections, _ := homeResp["sections"].([]interface{})
	foundTopicWithArt := false
	for _, s := range sections {
		sec := s.(map[string]interface{})
		if sec["id"] == "home:topics" {
			items := sec["items"].([]interface{})
			for _, it := range items {
				item := it.(map[string]interface{})
				if item["id"] == "100" && strings.HasPrefix(item["thumbnail_url"].(string), "https://") {
					foundTopicWithArt = true
				}
			}
		}
	}
	if !foundTopicWithArt {
		t.Errorf("expected home:topics to include topic 100 with valid https thumbnail")
	}
}


