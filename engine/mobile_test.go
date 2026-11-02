package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestMobileLifecycle(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "engine_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	filesDir := filepath.Join(tmpDir, "files")
	cacheDir := filepath.Join(tmpDir, "cache")

	cfg := map[string]interface{}{
		"auth_token": "mobile-test-token-12345",
	}
	cfgBytes, _ := json.Marshal(cfg)

	// 1. Start engine
	res, err := Start(filesDir, cacheDir, string(cfgBytes))
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if res.Port <= 0 {
		t.Fatalf("Start() returned invalid port: %d", res.Port)
	}
	if res.Token != "mobile-test-token-12345" {
		t.Fatalf("Start() returned unexpected token: %s", res.Token)
	}

	// 2. Health check
	healthJSON := Health()
	var healthMap map[string]interface{}
	if err := json.Unmarshal([]byte(healthJSON), &healthMap); err != nil {
		t.Fatalf("Health() returned invalid JSON: %s", healthJSON)
	}
	if healthMap["status"] != "running" {
		t.Errorf("Health status = %v, want 'running'", healthMap["status"])
	}

	// 3. Register a mock track
	dummyAudio := make([]byte, 65536)
	for i := range dummyAudio {
		dummyAudio[i] = byte(i % 128)
	}
	RegisterMockTrack("test_track_1", "Song Title", "Artist Name", 200, "audio/mpeg", dummyAudio)

	// 4. Request stream via GetStreamURL
	url := GetStreamURL("test_track_1")
	if url == "" {
		t.Fatal("GetStreamURL returned empty string")
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Stream request failed: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(data) != len(dummyAudio) {
		t.Errorf("streamed bytes = %d, want %d", len(data), len(dummyAudio))
	}

	// 5. Test direct chunk reading via GetTrackSize and ReadTrackChunk (Option A)
	trackSize := GetTrackSize("test_track_1")
	if trackSize != int64(len(dummyAudio)) {
		t.Errorf("GetTrackSize = %d, want %d", trackSize, len(dummyAudio))
	}

	// Read first 1024 bytes
	chunk1, err := ReadTrackChunk("test_track_1", 0, 1024)
	if err != nil {
		t.Fatalf("ReadTrackChunk(0, 1024) failed: %v", err)
	}
	if len(chunk1) != 1024 {
		t.Errorf("chunk1 len = %d, want 1024", len(chunk1))
	}
	for i := range chunk1 {
		if chunk1[i] != dummyAudio[i] {
			t.Fatalf("chunk1[%d] = %v, want %v", i, chunk1[i], dummyAudio[i])
		}
	}

	// Seek and read middle 2048 bytes
	chunk2, err := ReadTrackChunk("test_track_1", 4096, 2048)
	if err != nil {
		t.Fatalf("ReadTrackChunk(4096, 2048) failed: %v", err)
	}
	if len(chunk2) != 2048 {
		t.Errorf("chunk2 len = %d, want 2048", len(chunk2))
	}
	for i := range chunk2 {
		if chunk2[i] != dummyAudio[4096+i] {
			t.Fatalf("chunk2[%d] = %v, want %v", i, chunk2[i], dummyAudio[4096+i])
		}
	}

	// Read near end (boundary clamp)
	chunk3, err := ReadTrackChunk("test_track_1", 65530, 20)
	if err != nil {
		t.Fatalf("ReadTrackChunk(65530, 20) failed: %v", err)
	}
	if len(chunk3) != 6 { // only 6 bytes remaining to 65536
		t.Errorf("chunk3 len = %d, want 6", len(chunk3))
	}

	// Read beyond EOF
	chunkEOF, err := ReadTrackChunk("test_track_1", 70000, 100)
	if err != nil {
		t.Fatalf("ReadTrackChunk beyond EOF failed: %v", err)
	}
	if len(chunkEOF) != 0 {
		t.Errorf("chunkEOF len = %d, want 0", len(chunkEOF))
	}

	// 6. Stop engine
	Stop()


	// 6. Verify stopped
	healthAfter := Health()
	if healthAfter != `{"status":"stopped"}` {
		t.Errorf("Health() after Stop = %s, want stopped", healthAfter)
	}
}

