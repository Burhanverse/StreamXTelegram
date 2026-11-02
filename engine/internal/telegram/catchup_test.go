package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

func TestExtractAudioTrack(t *testing.T) {
	chatID := int64(1234567890)

	// Case 1: Audio document with full DocumentAttributeAudio
	msgAudio := &tg.Message{
		ID:   1001,
		Date: int(time.Now().Unix()),
		ReplyTo: &tg.MessageReplyHeader{
			ForumTopic:   true,
			ReplyToTopID: 555,
		},
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:            9001,
				AccessHash:    123456789,
				FileReference: []byte("ref_audio"),
				Size:          8 * 1024 * 1024,
				MimeType:      "audio/flac",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeAudio{
						Title:     "Bohemian Rhapsody",
						Performer: "Queen",
						Duration:  354,
					},
					&tg.DocumentAttributeFilename{
						FileName: "queen_bohemian.flac",
					},
				},
			},
		},
	}

	track, source, loc := ExtractAudioTrack(msgAudio, chatID)
	if track == nil || source == nil || loc == nil {
		t.Fatalf("ExtractAudioTrack returned nil for audio message")
	}

	if track.Title != "Bohemian Rhapsody" {
		t.Errorf("Expected title 'Bohemian Rhapsody', got '%s'", track.Title)
	}
	if track.Artist != "Queen" {
		t.Errorf("Expected artist 'Queen', got '%s'", track.Artist)
	}
	if track.DurationSec != 354 {
		t.Errorf("Expected duration 354s, got %d", track.DurationSec)
	}
	if track.FileSize != 8*1024*1024 {
		t.Errorf("Expected file size 8MB, got %d", track.FileSize)
	}
	if source.TopicID != 555 {
		t.Errorf("Expected topic ID 555, got %d", source.TopicID)
	}
	if loc.ID != 9001 || loc.AccessHash != 123456789 {
		t.Errorf("Location mismatch: %+v", loc)
	}

	// Case 2: Document with audio file extension but no audio attribute
	msgFile := &tg.Message{
		ID:   1002,
		Date: int(time.Now().Unix()),
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:            9002,
				AccessHash:    987654321,
				FileReference: []byte("ref_file"),
				Size:          4 * 1024 * 1024,
				MimeType:      "application/octet-stream",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeFilename{
						FileName: "awesome_song.mp3",
					},
				},
			},
		},
	}

	trackFile, sourceFile, _ := ExtractAudioTrack(msgFile, chatID)
	if trackFile == nil || sourceFile == nil {
		t.Fatalf("ExtractAudioTrack failed to infer audio from .mp3 extension")
	}
	if trackFile.Title != "awesome_song" {
		t.Errorf("Expected title 'awesome_song', got '%s'", trackFile.Title)
	}
	if trackFile.Artist != "Unknown Artist" {
		t.Errorf("Expected artist 'Unknown Artist', got '%s'", trackFile.Artist)
	}

	// Case 3: Non-audio document (PDF document)
	msgPDF := &tg.Message{
		ID:   1003,
		Date: int(time.Now().Unix()),
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:       9003,
				Size:     1024,
				MimeType: "application/pdf",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeFilename{FileName: "report.pdf"},
				},
			},
		},
	}

	trackPDF, _, _ := ExtractAudioTrack(msgPDF, chatID)
	if trackPDF != nil {
		t.Errorf("Expected nil for non-audio document, got %+v", trackPDF)
	}
}

func TestProcessMessageAndDBSync(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_catchup_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := database.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	primary := NewClientWorker(0, "123:token", nil, db)
	primary.Ready = true

	pool := &Pool{
		primaryWorker: primary,
		allWorkers:    []*ClientWorker{primary},
		db:            db,
		tracks:        make(map[string]*models.Track),
	}

	catchup := NewCatchupService(pool, db, nil)

	ctx := context.Background()
	channelID := int64(100200300)
	state := &models.SyncState{
		ChannelID: channelID,
		MaxMsgID:  0,
		Pts:       10,
	}

	msg := &tg.Message{
		ID:   50,
		Date: int(time.Now().Unix()),
		ReplyTo: &tg.MessageReplyHeader{
			ForumTopic:   true,
			ReplyToTopID: 77,
		},
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:            8888,
				AccessHash:    5544332211,
				FileReference: []byte("test_doc_ref"),
				Size:          3 * 1024 * 1024,
				MimeType:      "audio/mpeg",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeAudio{
						Title:     "Hotel California",
						Performer: "Eagles",
						Duration:  390,
					},
				},
			},
		},
	}

	catchup.processMessage(ctx, msg, channelID, state)

	if state.MaxMsgID != 50 {
		t.Errorf("Expected MaxMsgID 50, got %d", state.MaxMsgID)
	}

	// Verify track stored in SQLite
	trackID := "doc_8888"
	track, err := db.GetTrack(ctx, trackID)
	if err != nil || track == nil {
		t.Fatalf("Track not stored in DB: %v", err)
	}
	if track.Title != "Hotel California" || track.Artist != "Eagles" {
		t.Errorf("Track metadata mismatch: %+v", track)
	}

	// Verify track source stored in SQLite
	sources, err := db.GetTrackSources(ctx, trackID)
	if err != nil || len(sources) != 1 {
		t.Fatalf("Expected 1 track source, got %d (err: %v)", len(sources), err)
	}
	if sources[0].TopicID != 77 || sources[0].MsgID != 50 {
		t.Errorf("Source mismatch: %+v", sources[0])
	}

	// Verify worker location cached in SQLite
	loc, err := db.GetWorkerLocation(ctx, primary.BotIDString(), 50)
	if err != nil || loc == nil {
		t.Fatalf("Worker location not stored in DB: %v", err)
	}
	if loc.AccessHash != 5544332211 {
		t.Errorf("Access hash mismatch: %d", loc.AccessHash)
	}
}

func TestParsePlaylistsMessage(t *testing.T) {
	msg := `#playlist

name: Hindi
topicid: 3
name: English 
topicid: 2
`
	mapping := ParsePlaylistsMessage(msg)
	if len(mapping) != 2 {
		t.Fatalf("Expected 2 playlist mappings, got %d", len(mapping))
	}
	if mapping[3] != "Hindi" {
		t.Errorf("Expected 'Hindi' for topic 3, got '%s'", mapping[3])
	}
	if mapping[2] != "English" {
		t.Errorf("Expected 'English' for topic 2, got '%s'", mapping[2])
	}

	// Test reversed order: topicid before name
	msgReversed := `#playlists
topicid: 10
name: Chill Beats
`
	mapping2 := ParsePlaylistsMessage(msgReversed)
	if mapping2[10] != "Chill Beats" {
		t.Errorf("Expected 'Chill Beats' for topic 10, got '%s'", mapping2[10])
	}
}
