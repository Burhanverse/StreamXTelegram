package stream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamDiagnosticsDisabled(t *testing.T) {
	sd := NewStreamDiagnostics(false, "")
	if sd.IsEnabled() {
		t.Fatal("expected diagnostics to be disabled")
	}
	sess := sd.StartSession("track1")
	if sess != nil {
		t.Fatal("expected nil session when disabled")
	}
	sd.RecordRead("track1", 1024, 10*time.Millisecond, 1024)
	sd.RecordCacheAccess("track1", true)
	sd.RecordChunkRequest("track1", 0)
	sd.RecordBotDownload("track1", 0, 50*time.Millisecond, nil)
	sd.RecordFloodWait("track1", 0, 2*time.Second)
	sd.RecordError("track1", "FILE_MIGRATE")
	sd.RecordBackgroundRead()
	sd.ReleaseSession("track1")

	stats := sd.GetRecentStats(10)
	if len(stats) != 0 {
		t.Fatalf("expected 0 stats lines when disabled, got %d", len(stats))
	}
}

func TestStreamDiagnosticsEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	sd := NewStreamDiagnostics(true, tmpDir)
	if !sd.IsEnabled() {
		t.Fatal("expected diagnostics to be enabled")
	}

	trackID := "test_track_123"
	sess := sd.StartSession(trackID)
	if sess == nil {
		t.Fatal("expected non-nil session")
	}

	// 1. Record onRead calls with varying sizes and latencies
	sd.RecordRead(trackID, 32768, 120*time.Millisecond, 32768) // TTFB
	sd.RecordRead(trackID, 65536, 40*time.Millisecond, 65536)
	sd.RecordRead(trackID, 131072, 600*time.Millisecond, 131072) // >500ms blocked

	// 2. Record cache hits and misses
	sd.RecordCacheAccess(trackID, true)
	sd.RecordCacheAccess(trackID, true)
	sd.RecordCacheAccess(trackID, false)

	// 3. Record chunk requests (simulate duplicate prefetch)
	sd.RecordChunkRequest(trackID, 0)
	sd.RecordChunkRequest(trackID, 1048576)
	sd.RecordChunkRequest(trackID, 1048576) // Duplicate!

	// 4. Record bot latencies
	sd.RecordBotDownload(trackID, 0, 100*time.Millisecond, nil)
	sd.RecordBotDownload(trackID, 0, 200*time.Millisecond, nil)
	sd.RecordBotDownload(trackID, 0, 300*time.Millisecond, nil)
	sd.RecordBotDownload(trackID, 1, 150*time.Millisecond, nil)

	// 5. Record flood wait
	sd.RecordFloodWait(trackID, 1, 12*time.Second)

	// 6. Record errors
	sd.RecordError(trackID, "FILE_MIGRATE")
	sd.RecordError(trackID, "FILE_REFERENCE_EXPIRED")

	// 7. Record background read
	sd.RecordBackgroundRead()

	// Verify session internal values
	sess.mu.Lock()
	if sess.OnReadCount != 3 {
		t.Fatalf("expected OnReadCount 3, got %d", sess.OnReadCount)
	}
	if !sess.hasTTFB || sess.TTFB != 120*time.Millisecond {
		t.Fatalf("expected TTFB 120ms, got %v", sess.TTFB)
	}
	if sess.BlockedOver500ms != 1 {
		t.Fatalf("expected 1 blocked read, got %d", sess.BlockedOver500ms)
	}
	if sess.ChunkRequestsIssued != 3 {
		t.Fatalf("expected 3 chunk requests, got %d", sess.ChunkRequestsIssued)
	}
	if len(sess.UniqueChunks) != 2 {
		t.Fatalf("expected 2 unique chunks, got %d", len(sess.UniqueChunks))
	}
	if sess.CacheHits != 2 || sess.CacheMisses != 1 {
		t.Fatalf("expected 2 hits, 1 miss; got %d, %d", sess.CacheHits, sess.CacheMisses)
	}
	if sess.FloodWaitCounts[1] != 1 || sess.FloodWaitSleptSec[1] != 12.0 {
		t.Fatalf("unexpected flood stats: %+v, %+v", sess.FloodWaitCounts, sess.FloodWaitSleptSec)
	}
	if sess.FileMigrateErrors != 1 || sess.FileRefExpiredErrors != 1 {
		t.Fatalf("unexpected errors: migrate=%d, ref=%d", sess.FileMigrateErrors, sess.FileRefExpiredErrors)
	}
	if sess.BackgroundReadsCount != 1 {
		t.Fatalf("expected 1 background read, got %d", sess.BackgroundReadsCount)
	}
	sess.mu.Unlock()

	// Rolling line test
	sess.LogRollingLine()

	// Release session
	sd.ReleaseSession(trackID)

	// Check that release summary line and rolling line were captured
	stats := sd.GetRecentStats(10)
	if len(stats) < 2 {
		t.Fatalf("expected at least 2 log lines, got %d", len(stats))
	}

	releaseLine := stats[len(stats)-1]
	if !strings.Contains(releaseLine, "[STREAM-STATS] [RELEASE]") {
		t.Fatalf("expected release line tag, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "reads=3") {
		t.Fatalf("expected reads=3, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "sizes(min/med/max)=32768/65536/131072") {
		t.Fatalf("expected correct size histogram, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "chunks(issued/unique)=3/2") {
		t.Fatalf("expected chunks(issued/unique)=3/2, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "cache(hit/miss)=2/1") {
		t.Fatalf("expected cache(hit/miss)=2/1, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "blocked_500ms=1") {
		t.Fatalf("expected blocked_500ms=1, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "bot#0:") || !strings.Contains(releaseLine, "bot#1:") {
		t.Fatalf("expected bot latencies, got: %s", releaseLine)
	}
	if !strings.Contains(releaseLine, "bg_reads=1") {
		t.Fatalf("expected bg_reads=1, got: %s", releaseLine)
	}

	// Verify log file was written
	logPath := filepath.Join(tmpDir, "logs", "stream_stats.log")
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("expected log file to exist and have contents: %v", err)
	}
}

func TestRotatedLogFileCapAndRotation(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	// Cap at 100 bytes
	rf := NewRotatedLogFile(logPath, 100)

	// Write 60 bytes
	line1 := strings.Repeat("A", 50)
	if err := rf.Append(line1); err != nil {
		t.Fatalf("append failed: %v", err)
	}

	// Write another 60 bytes -> total > 100 bytes -> must rotate to test.log.1
	line2 := strings.Repeat("B", 50)
	if err := rf.Append(line2); err != nil {
		t.Fatalf("append failed: %v", err)
	}

	// Check that backup file exists
	backupPath := logPath + ".1"
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("expected backup file %s to exist: %v", backupPath, err)
	}
}

