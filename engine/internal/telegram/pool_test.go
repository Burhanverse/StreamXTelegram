package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

func TestPoolWorkerSelection(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_pool_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := database.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	w0 := NewClientWorker(0, "111:token_primary", nil, db)
	w0.Ready = true
	w1 := NewClientWorker(1, "222:token_worker_1", nil, db)
	w1.Ready = true
	w2 := NewClientWorker(2, "333:token_worker_2", nil, db)
	w2.Ready = true

	pool := &Pool{
		primaryWorker:    w0,
		secondaryWorkers: []*ClientWorker{w1, w2},
		allWorkers:       []*ClientWorker{w0, w1, w2},
		db:               db,
		tracks:           make(map[string]*models.Track),
	}

	// Case 1: All loads are 0, selects first worker (w0)
	selected := pool.AcquireWorker()
	if selected.Index != 0 {
		t.Errorf("Expected worker 0 with 0 load, got worker %d", selected.Index)
	}

	// Case 2: w0 has load 5, w1 has load 2, w2 has load 8 -> Should pick w1
	atomic.StoreInt64(&w0.Workload, 5)
	atomic.StoreInt64(&w1.Workload, 2)
	atomic.StoreInt64(&w2.Workload, 8)

	selected = pool.AcquireWorker()
	if selected.Index != 1 {
		t.Errorf("Expected worker 1 (lowest load 2), got worker %d (load %d)", selected.Index, selected.Workload)
	}

	// Case 3: Worker 1 goes offline (Ready = false) -> Should pick w0 (load 5 < 8)
	w1.Ready = false
	selected = pool.AcquireWorker()
	if selected.Index != 0 {
		t.Errorf("Expected worker 0 when worker 1 is offline, got worker %d", selected.Index)
	}
}

func TestWorkerLocationCache(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "musix_loc_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	db, err := database.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer db.Close()

	w := NewClientWorker(0, "12345:token", nil, db)
	w.Ready = true

	ctx := context.Background()
	msgID := int32(4242)

	// In-memory caching and persistent DB retrieval
	loc := &models.WorkerLocation{
		BotID:         w.BotIDString(),
		MsgID:         msgID,
		AccessHash:    9988776655,
		FileReference: []byte("test_file_reference_bytes"),
		DCID:          2,
		UpdatedAt:     time.Now(),
	}

	if err := db.SaveWorkerLocation(ctx, loc); err != nil {
		t.Fatalf("Failed to save worker location: %v", err)
	}

	fetched, err := db.GetWorkerLocation(ctx, w.BotIDString(), msgID)
	if err != nil {
		t.Fatalf("Failed to get worker location: %v", err)
	}
	if fetched == nil || fetched.AccessHash != 9988776655 {
		t.Fatalf("Worker location mismatch: %+v", fetched)
	}
}

func TestChannelAccessHashCache(t *testing.T) {
	w := NewClientWorker(0, "token", nil, nil)
	channelID := int64(1234567890)
	hash := int64(9876543210123)

	if got := w.GetChannelAccessHash(channelID); got != 0 {
		t.Errorf("Expected 0 before set, got %d", got)
	}

	w.SetChannelAccessHash(channelID, hash)

	if got := w.GetChannelAccessHash(channelID); got != hash {
		t.Errorf("Expected hash %d, got %d", hash, got)
	}
}

func TestChunkAlignmentCalculation(t *testing.T) {
	const oneMB int64 = 1024 * 1024

	// Test 1: Offset not aligned to 4KB (e.g. 5000)
	var offset int64 = 5000
	alignedOffset := (offset / 4096) * 4096
	skipBytes := offset - alignedOffset

	if alignedOffset != 4096 {
		t.Errorf("Expected aligned offset 4096, got %d", alignedOffset)
	}
	if skipBytes != (5000 - 4096) {
		t.Errorf("Expected skip bytes %d, got %d", 5000-4096, skipBytes)
	}

	// Test 2: Chunk boundary capping at 1MB
	// Offset: 800 KB, Requested Limit: 512 KB
	// Boundary at 1MB is 224 KB away (1024 - 800 = 224 KB)
	var chunkOffset int64 = 800 * 1024
	limit := 512 * 1024
	bytesToBoundary := oneMB - (chunkOffset % oneMB)
	if int64(limit) > bytesToBoundary {
		limit = int(bytesToBoundary)
	}

	if limit != 224*1024 {
		t.Errorf("Expected capped limit %d, got %d", 224*1024, limit)
	}
}

func TestFloodWaitAndSecretRedaction(t *testing.T) {
	// ParseFloodWait
	_, ok := ParseFloodWait(nil)
	if ok {
		t.Error("Expected false for nil error")
	}

	wait, ok := ParseFloodWait(errors.New("rpc error: code 420 message FLOOD_WAIT_18"))
	if !ok || wait != 18*time.Second {
		t.Errorf("Expected 18s wait, got %v (ok: %t)", wait, ok)
	}

	wait, ok = ParseFloodWait(errors.New("rpc error: code 420 message FLOOD_PREMIUM_WAIT_5"))
	if !ok || wait != 5*time.Second {
		t.Errorf("Expected 5s wait, got %v (ok: %t)", wait, ok)
	}

	// RedactSecret
	token := "123456789:ABCdefGHIjklMNOpqrsTUVwxyz123456"
	redacted := RedactSecret(token)
	if redacted == token {
		t.Errorf("Secret was not redacted: %s", redacted)
	}
	if redacted != "1234...[REDACTED]" {
		t.Errorf("Expected '1234...[REDACTED]', got '%s'", redacted)
	}
}

func TestNormalizeChannelID(t *testing.T) {
	// -100 prefix format
	id, isChannel := normalizeChannelID(-1001234567890)
	if !isChannel || id != 1234567890 {
		t.Errorf("Expected 1234567890 true, got %d %t", id, isChannel)
	}

	// Plain positive channel ID
	id, isChannel = normalizeChannelID(1234567890)
	if !isChannel || id != 1234567890 {
		t.Errorf("Expected 1234567890 true, got %d %t", id, isChannel)
	}

	// Small negative chat ID (not a channel/supergroup)
	id, isChannel = normalizeChannelID(-54321)
	if isChannel {
		t.Errorf("Expected false for normal group chat ID -54321, got %d %t", id, isChannel)
	}
}
