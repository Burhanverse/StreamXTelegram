package stream

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

type fakeDownloadRecord struct {
	Offset int64
	Limit  int
}

type fakeTelegramClient struct {
	mu            sync.Mutex
	tracks        map[string]*models.Track
	downloads     []fakeDownloadRecord
	downloadCount int32
	latency       time.Duration
	errorFunc     func(offset int64, limit int) error
	dataGen       func(offset int64, limit int) []byte
}

func newFakeTelegramClient() *fakeTelegramClient {
	return &fakeTelegramClient{
		tracks: make(map[string]*models.Track),
		dataGen: func(offset int64, limit int) []byte {
			buf := make([]byte, limit)
			for i := range buf {
				buf[i] = byte((offset + int64(i)) % 256)
			}
			return buf
		},
	}
}

func (f *fakeTelegramClient) Start(ctx context.Context) error { return nil }
func (f *fakeTelegramClient) Stop()                          {}
func (f *fakeTelegramClient) IsReady() bool                  { return true }

func (f *fakeTelegramClient) RegisterTrack(t *models.Track) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracks[t.ID] = t
}

func (f *fakeTelegramClient) ResolveTrack(ctx context.Context, trackID string) (*models.Track, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tracks[trackID]
	if !ok {
		return nil, fmt.Errorf("track not found: %s", trackID)
	}
	return t, nil
}

func (f *fakeTelegramClient) DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	atomic.AddInt32(&f.downloadCount, 1)

	f.mu.Lock()
	f.downloads = append(f.downloads, fakeDownloadRecord{Offset: offset, Limit: limit})
	errFn := f.errorFunc
	delay := f.latency
	gen := f.dataGen
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	if errFn != nil {
		if err := errFn(offset, limit); err != nil {
			return nil, err
		}
	}

	return gen(offset, limit), nil
}

// (a) 20 sequential 64 KB reads of one 1 MB chunk cause exactly one download
func TestPrompt1_SequentialReadsSingleDownload(t *testing.T) {
	fakeClient := newFakeTelegramClient()
	track := &models.Track{
		ID:       "track_seq_test",
		Title:    "Test Song",
		FileSize: 10 * 1024 * 1024,
		DurationSec: 300,
	}
	fakeClient.RegisterTrack(track)

	streamer := NewStreamer(fakeClient)
	ctx := context.Background()

	handle, err := streamer.OpenStream(ctx, track.ID)
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer streamer.CloseStream(handle)

	// Offset at 1 MB (so it's a full 1 MB chunk, aligned at 1048576)
	baseOffset := int64(1024 * 1024)
	chunkSize := 64 * 1024

	// Read 20 sequential 64 KB pieces (total 1280 KB, first 16 pieces span the first 1 MB chunk)
	// We read specifically 16 pieces to cover the exact 1 MB chunk (16 * 64 KB = 1024 KB)
	for i := 0; i < 16; i++ {
		offset := baseOffset + int64(i*chunkSize)
		data, err := streamer.ReadStream(handle, offset, chunkSize)
		if err != nil {
			t.Fatalf("ReadStream at offset %d failed: %v", offset, err)
		}
		if len(data) != chunkSize {
			t.Fatalf("ReadStream returned %d bytes, expected %d", len(data), chunkSize)
		}
	}

	// Count how many times the chunk at baseOffset was downloaded
	fakeClient.mu.Lock()
	var chunk1MDownloads int
	for _, dl := range fakeClient.downloads {
		if dl.Offset == baseOffset {
			chunk1MDownloads++
		}
	}
	fakeClient.mu.Unlock()

	if chunk1MDownloads != 1 {
		t.Errorf("Expected exactly 1 download for chunk at %d, but got %d", baseOffset, chunk1MDownloads)
	}
}

// (b) Seek cancels obsolete prefetch
func TestPrompt1_SeekCancelsObsoletePrefetch(t *testing.T) {
	fakeClient := newFakeTelegramClient()
	fakeClient.latency = 200 * time.Millisecond // Slow download so prefetch stays in flight
	track := &models.Track{
		ID:       "track_seek_test",
		Title:    "Seek Song",
		FileSize: 20 * 1024 * 1024,
		DurationSec: 300,
	}
	fakeClient.RegisterTrack(track)

	streamer := NewStreamer(fakeClient)
	ctx := context.Background()

	handle, err := streamer.OpenStream(ctx, track.ID)
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer streamer.CloseStream(handle)

	// Initial small read at offset 0
	_, err = streamer.ReadStream(handle, 0, 1024)
	if err != nil {
		t.Fatalf("Initial read failed: %v", err)
	}

	// Allow prefetch goroutines to spawn
	time.Sleep(50 * time.Millisecond)

	// Seek jump far away to 15 MB
	seekOffset := int64(15 * 1024 * 1024)
	_, err = streamer.ReadStream(handle, seekOffset, 1024)
	if err != nil {
		t.Fatalf("Seek read failed: %v", err)
	}

	// Verify session prefetches outside the new window were cancelled
	streamer.sessionsMu.RLock()
	sess := streamer.sessions[handle]
	streamer.sessionsMu.RUnlock()

	sess.mu.Lock()
	cancelsRemaining := len(sess.prefetchCancels)
	sess.mu.Unlock()

	// Prefetches from near offset 0 must have been pruned/cancelled
	if cancelsRemaining > 3 {
		t.Errorf("Expected cancelled prefetches after seek, remaining: %d", cancelsRemaining)
	}
}

// (c) Flooded worker is skipped and playback continues
func TestPrompt1_FloodedWorkerSkipped(t *testing.T) {
	cfg := &models.Config{
		ApiID:        12345,
		ApiHash:      "hash",
		WorkerCount:  2,
		BotToken:     "111:token1",
		WorkerTokens: []string{"222:token2"},
		FilesDir:     t.TempDir(),
	}

	pool, err := telegram.NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	workers := pool.Workers()
	if len(workers) < 2 {
		t.Fatalf("Expected at least 2 workers, got %d", len(workers))
	}

	w0 := workers[0]
	w1 := workers[1]

	w0.Ready = true
	w1.Ready = true

	// Mark worker 0 as cooling down due to FLOOD_WAIT 30s
	w0.MarkCooldown(30 * time.Second)

	// Acquire worker with playback priority
	selected := pool.AcquireWorkerWithPriority(models.PriorityPlayback)
	if selected == nil {
		t.Fatal("AcquireWorker returned nil")
	}

	// Assert flooded worker 0 is skipped and worker 1 is selected
	if selected.Index != w1.Index {
		t.Errorf("Expected worker %d to be selected, but got worker %d", w1.Index, selected.Index)
	}

	// Clear cooldown
	w0.MarkCooldown(0)
}

// (d) -race clean verification with concurrent reads and seeks
func TestPrompt1_RaceCleanConcurrentReads(t *testing.T) {
	fakeClient := newFakeTelegramClient()
	track := &models.Track{
		ID:       "track_race_test",
		Title:    "Race Song",
		FileSize: 10 * 1024 * 1024,
		DurationSec: 240,
	}
	fakeClient.RegisterTrack(track)

	streamer := NewStreamer(fakeClient)
	ctx := context.Background()

	handle, err := streamer.OpenStream(ctx, track.ID)
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer streamer.CloseStream(handle)

	var wg sync.WaitGroup
	// Run 8 concurrent reading goroutines at various offsets
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(routineIdx int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				offset := int64((routineIdx*100 + j*64) * 1024)
				_, _ = streamer.ReadStream(handle, offset, 16*1024)
			}
		}(i)
	}

	wg.Wait()
}

// (e) Release stops all session goroutines
func TestPrompt1_ReleaseStopsGoroutines(t *testing.T) {
	fakeClient := newFakeTelegramClient()
	fakeClient.latency = 100 * time.Millisecond
	track := &models.Track{
		ID:       "track_goroutine_test",
		Title:    "Goroutine Song",
		FileSize: 15 * 1024 * 1024,
		DurationSec: 300,
	}
	fakeClient.RegisterTrack(track)

	streamer := NewStreamer(fakeClient)
	ctx := context.Background()

	initialGoroutines := runtime.NumGoroutine()

	handle, err := streamer.OpenStream(ctx, track.ID)
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	// Read to trigger prefetch goroutines
	_, _ = streamer.ReadStream(handle, 0, 32*1024)

	// Close session
	streamer.CloseStream(handle)

	// Wait briefly for goroutines to terminate
	deadline := time.Now().Add(1 * time.Second)
	var finalGoroutines int
	for time.Now().Before(deadline) {
		runtime.Gosched()
		finalGoroutines = runtime.NumGoroutine()
		if finalGoroutines <= initialGoroutines+2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalGoroutines > initialGoroutines+5 {
		t.Errorf("Potential goroutine leak: initial %d, after release %d", initialGoroutines, finalGoroutines)
	}
}

// Prompt 2: ReadHeader does not prefetch or pollute playback cache
func TestPrompt2_NoPrefetchFromReadHeader(t *testing.T) {
	fakeClient := newFakeTelegramClient()
	track := &models.Track{
		ID:          "track_header_test",
		Title:       "Header Song",
		FileSize:    10 * 1024 * 1024,
		DurationSec: 250,
	}
	fakeClient.RegisterTrack(track)

	streamer := NewStreamer(fakeClient)
	ctx := context.Background()

	// Perform header read
	headerData, err := streamer.ReadHeader(ctx, track.ID, 256*1024)
	if err != nil {
		t.Fatalf("ReadHeader failed: %v", err)
	}
	if len(headerData) != 256*1024 {
		t.Fatalf("Expected 256 KB header, got %d bytes", len(headerData))
	}

	// Verify prefetch cache was NOT polluted by ReadHeader
	_, inCache := streamer.prefetchCache.Get(track.ID, 0, 256*1024)
	if inCache {
		t.Errorf("Expected ReadHeader to NOT store chunk in playback prefetch cache")
	}

	// Exactly 1 download request was issued, zero prefetch downloads
	count := atomic.LoadInt32(&fakeClient.downloadCount)
	if count != 1 {
		t.Errorf("Expected exactly 1 download for ReadHeader, got %d", count)
	}
}
