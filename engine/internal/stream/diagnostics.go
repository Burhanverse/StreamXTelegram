package stream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"musix/engine/internal/logger"
	"musix/engine/internal/models"
)

var logStats = logger.New("stream-stats")

// RingBuffer stores a bounded list of recent log lines in FIFO order.
type RingBuffer struct {
	mu       sync.RWMutex
	capacity int
	lines    []string
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		capacity: capacity,
		lines:    make([]string, 0, capacity),
	}
}

func (rb *RingBuffer) Add(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.lines) >= rb.capacity {
		rb.lines = rb.lines[1:]
	}
	rb.lines = append(rb.lines, line)
}

func (rb *RingBuffer) Get(limit int) []string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	n := len(rb.lines)
	if limit <= 0 || limit > n {
		limit = n
	}
	start := n - limit
	result := make([]string, limit)
	copy(result, rb.lines[start:])
	return result
}

// RotatedLogFile manages an append-only log file capped at maxBytes (5 MB) with rotation (.1).
type RotatedLogFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
}

func NewRotatedLogFile(path string, maxBytes int64) *RotatedLogFile {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	return &RotatedLogFile{
		path:     path,
		maxBytes: maxBytes,
	}
}

func (rf *RotatedLogFile) Append(line string) error {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	data := []byte(line + "\n")
	fi, err := os.Stat(rf.path)
	if err == nil && fi.Size()+int64(len(data)) > rf.maxBytes {
		backupPath := rf.path + ".1"
		_ = os.Remove(backupPath)
		_ = os.Rename(rf.path, backupPath)
	}

	f, err := os.OpenFile(rf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

// PlaybackSession tracks live metrics for a single track playback.
type PlaybackSession struct {
	TrackID             string
	StartTime           time.Time
	FirstReadStart      time.Time
	TTFB                time.Duration
	hasTTFB             bool

	OnReadCount         int64
	ReadSizes           []int
	BlockedOver500ms    int64

	ChunkRequestsIssued int64
	UniqueChunks        map[int64]bool
	CacheHits           int64
	CacheMisses         int64

	BotLatencies        map[int][]time.Duration
	FloodWaitCounts     map[int]int64
	FloodWaitSleptSec   map[int]float64

	FileMigrateErrors   int64
	FileRefExpiredErrors int64
	BackgroundReadsCount int64

	closed              bool
	done                chan struct{}
	mu                  sync.Mutex
	diagnostics         *StreamDiagnostics
}

func (s *PlaybackSession) LogRollingLine() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	elapsed := time.Since(s.StartTime)
	line := s.formatLineLocked("ROLLING", elapsed)
	s.mu.Unlock()

	s.diagnostics.recordLine(line)
}

func (s *PlaybackSession) Release() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	elapsed := time.Since(s.StartTime)
	line := s.formatLineLocked("RELEASE", elapsed)
	s.mu.Unlock()

	s.diagnostics.recordLine(line)
}

func computeMinMedMax(sizes []int) (int, int, int) {
	if len(sizes) == 0 {
		return 0, 0, 0
	}
	sorted := make([]int, len(sizes))
	copy(sorted, sizes)
	sort.Ints(sorted)
	return sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1]
}

func computeP50P95(durations []time.Duration) (time.Duration, time.Duration) {
	if len(durations) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[len(sorted)*50/100]
	p95 := sorted[len(sorted)*95/100]
	return p50, p95
}

func (s *PlaybackSession) formatLineLocked(tag string, elapsed time.Duration) string {
	minSize, medSize, maxSize := computeMinMedMax(s.ReadSizes)

	ttfbStr := "n/a"
	if s.hasTTFB {
		ttfbStr = fmt.Sprintf("%v", s.TTFB.Round(time.Millisecond))
	}

	var botLatParts []string
	var botIndices []int
	for idx := range s.BotLatencies {
		botIndices = append(botIndices, idx)
	}
	sort.Ints(botIndices)
	for _, idx := range botIndices {
		lats := s.BotLatencies[idx]
		if len(lats) > 0 {
			p50, p95 := computeP50P95(lats)
			botLatParts = append(botLatParts, fmt.Sprintf("bot#%d: p50=%v p95=%v (n=%d)", idx, p50.Round(time.Millisecond), p95.Round(time.Millisecond), len(lats)))
		}
	}
	botLatStr := "none"
	if len(botLatParts) > 0 {
		botLatStr = strings.Join(botLatParts, ", ")
	}

	var floodParts []string
	var floodIndices []int
	for idx := range s.FloodWaitCounts {
		floodIndices = append(floodIndices, idx)
	}
	sort.Ints(floodIndices)
	for _, idx := range floodIndices {
		cnt := s.FloodWaitCounts[idx]
		sec := s.FloodWaitSleptSec[idx]
		floodParts = append(floodParts, fmt.Sprintf("bot#%d: %d (%.1fs)", idx, cnt, sec))
	}
	floodStr := "none"
	if len(floodParts) > 0 {
		floodStr = strings.Join(floodParts, ", ")
	}

	return fmt.Sprintf(
		"[STREAM-STATS] [%s] track=%s elapsed=%.1fs reads=%d sizes(min/med/max)=%d/%d/%d chunks(issued/unique)=%d/%d cache(hit/miss)=%d/%d ttfb=%s blocked_500ms=%d bot_latency={%s} flood={%s} errors={FILE_MIGRATE: %d, FILE_REF_EXPIRED: %d} bg_reads=%d",
		tag,
		s.TrackID,
		elapsed.Seconds(),
		s.OnReadCount,
		minSize, medSize, maxSize,
		s.ChunkRequestsIssued,
		len(s.UniqueChunks),
		s.CacheHits,
		s.CacheMisses,
		ttfbStr,
		s.BlockedOver500ms,
		botLatStr,
		floodStr,
		s.FileMigrateErrors,
		s.FileRefExpiredErrors,
		s.BackgroundReadsCount,
	)
}

// StreamDiagnostics manages per-track playback sessions and logging.
type StreamDiagnostics struct {
	enabled    bool
	mu         sync.RWMutex
	sessions   map[string]*PlaybackSession
	ringBuffer *RingBuffer
	logFile    *RotatedLogFile
}

func NewStreamDiagnostics(enabled bool, filesDir string) *StreamDiagnostics {
	var rf *RotatedLogFile
	if filesDir != "" {
		logPath := filepath.Join(filesDir, "logs", "stream_stats.log")
		rf = NewRotatedLogFile(logPath, 5*1024*1024)
	}
	return &StreamDiagnostics{
		enabled:    enabled,
		sessions:   make(map[string]*PlaybackSession),
		ringBuffer: NewRingBuffer(200),
		logFile:    rf,
	}
}

func (sd *StreamDiagnostics) IsEnabled() bool {
	if sd == nil {
		return false
	}
	return sd.enabled
}

func (sd *StreamDiagnostics) recordLine(line string) {
	logStats.Infof("%s", line)
	if sd.ringBuffer != nil {
		sd.ringBuffer.Add(line)
	}
	if sd.logFile != nil {
		_ = sd.logFile.Append(line)
	}
}

// StartSession initiates or retrieves a playback session for trackID.
func (sd *StreamDiagnostics) StartSession(trackID string) *PlaybackSession {
	if sd == nil || !sd.enabled || trackID == "" {
		return nil
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()

	if sess, exists := sd.sessions[trackID]; exists && !sess.closed {
		return sess
	}

	sess := &PlaybackSession{
		TrackID:           trackID,
		StartTime:         time.Now(),
		UniqueChunks:      make(map[int64]bool),
		BotLatencies:      make(map[int][]time.Duration),
		FloodWaitCounts:   make(map[int]int64),
		FloodWaitSleptSec: make(map[int]float64),
		done:              make(chan struct{}),
		diagnostics:       sd,
	}
	sd.sessions[trackID] = sess

	// Rolling stats logger every 10 seconds
	go func(s *PlaybackSession) {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.LogRollingLine()
			case <-s.done:
				return
			}
		}
	}(sess)

	return sess
}

// GetSession returns the active session for trackID, or creates one if enabled.
func (sd *StreamDiagnostics) GetSession(trackID string) *PlaybackSession {
	if sd == nil || !sd.enabled || trackID == "" {
		return nil
	}
	sd.mu.RLock()
	sess, exists := sd.sessions[trackID]
	sd.mu.RUnlock()
	if exists && !sess.closed {
		return sess
	}
	return sd.StartSession(trackID)
}

// ReleaseSession marks the playback session for trackID released and logs the summary.
func (sd *StreamDiagnostics) ReleaseSession(trackID string) {
	if sd == nil || !sd.enabled || trackID == "" {
		return
	}
	sd.mu.Lock()
	sess, exists := sd.sessions[trackID]
	if exists {
		delete(sd.sessions, trackID)
	}
	sd.mu.Unlock()

	if sess != nil {
		sess.Release()
	}
}

// RecordRead records an onRead invocation.
func (sd *StreamDiagnostics) RecordRead(trackID string, length int, duration time.Duration, bytesRead int) {
	sess := sd.GetSession(trackID)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()

	sess.OnReadCount++
	sess.ReadSizes = append(sess.ReadSizes, length)
	if duration > 500*time.Millisecond {
		sess.BlockedOver500ms++
	}

	if !sess.hasTTFB && bytesRead > 0 {
		sess.hasTTFB = true
		sess.TTFB = duration
	}
}

// RecordCacheAccess records a prefetch cache hit or miss.
func (sd *StreamDiagnostics) RecordCacheAccess(trackID string, hit bool) {
	sess := sd.GetSession(trackID)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if hit {
		sess.CacheHits++
	} else {
		sess.CacheMisses++
	}
}

// RecordChunkRequest records that a chunk download was issued.
func (sd *StreamDiagnostics) RecordChunkRequest(trackID string, offset int64) {
	sess := sd.GetSession(trackID)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.ChunkRequestsIssued++
	sess.UniqueChunks[offset] = true
}

// WithTrackID decorates ctx with the active trackID for telemetry tracking.
func WithTrackID(ctx context.Context, trackID string) context.Context {
	return models.WithTrackID(ctx, trackID)
}

// TrackIDFromContext retrieves the trackID from ctx if present.
func TrackIDFromContext(ctx context.Context) string {
	return models.TrackIDFromContext(ctx)
}

// WithBackground marks ctx as a background/enrichment operation.
func WithBackground(ctx context.Context) context.Context {
	return models.WithBackground(ctx)
}

// IsBackgroundContext checks if ctx was marked as a background operation.
func IsBackgroundContext(ctx context.Context) bool {
	return models.IsBackgroundContext(ctx)
}

// RecordBotDownload records download latency and errors for a specific bot index.
func (sd *StreamDiagnostics) RecordBotDownload(trackID string, botIndex int, duration time.Duration, err error) {
	if sd == nil || !sd.enabled {
		return
	}
	if trackID != "" {
		if sess := sd.GetSession(trackID); sess != nil {
			sess.mu.Lock()
			sess.BotLatencies[botIndex] = append(sess.BotLatencies[botIndex], duration)
			if err != nil {
				errStr := strings.ToUpper(err.Error())
				if strings.Contains(errStr, "FILE_MIGRATE") {
					sess.FileMigrateErrors++
				}
				if strings.Contains(errStr, "FILE_REFERENCE") {
					sess.FileRefExpiredErrors++
				}
			}
			sess.mu.Unlock()
			return
		}
	}
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, sess := range sd.sessions {
		sess.mu.Lock()
		sess.BotLatencies[botIndex] = append(sess.BotLatencies[botIndex], duration)
		if err != nil {
			errStr := strings.ToUpper(err.Error())
			if strings.Contains(errStr, "FILE_MIGRATE") {
				sess.FileMigrateErrors++
			}
			if strings.Contains(errStr, "FILE_REFERENCE") {
				sess.FileRefExpiredErrors++
			}
		}
		sess.mu.Unlock()
	}
}

// RecordFloodWait records a FLOOD_WAIT event for a bot index.
func (sd *StreamDiagnostics) RecordFloodWait(trackID string, botIndex int, wait time.Duration) {
	if sd == nil || !sd.enabled {
		return
	}
	if trackID != "" {
		if sess := sd.GetSession(trackID); sess != nil {
			sess.mu.Lock()
			sess.FloodWaitCounts[botIndex]++
			sess.FloodWaitSleptSec[botIndex] += wait.Seconds()
			sess.mu.Unlock()
			return
		}
	}
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, sess := range sd.sessions {
		sess.mu.Lock()
		sess.FloodWaitCounts[botIndex]++
		sess.FloodWaitSleptSec[botIndex] += wait.Seconds()
		sess.mu.Unlock()
	}
}

// RecordError records FILE_MIGRATE or FILE_REFERENCE_EXPIRED errors directly.
func (sd *StreamDiagnostics) RecordError(trackID string, errType string) {
	if sd == nil || !sd.enabled {
		return
	}
	if trackID != "" {
		if sess := sd.GetSession(trackID); sess != nil {
			sess.mu.Lock()
			if strings.Contains(errType, "FILE_MIGRATE") {
				sess.FileMigrateErrors++
			}
			if strings.Contains(errType, "FILE_REFERENCE") {
				sess.FileRefExpiredErrors++
			}
			sess.mu.Unlock()
			return
		}
	}
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, sess := range sd.sessions {
		sess.mu.Lock()
		if strings.Contains(errType, "FILE_MIGRATE") {
			sess.FileMigrateErrors++
		}
		if strings.Contains(errType, "FILE_REFERENCE") {
			sess.FileRefExpiredErrors++
		}
		sess.mu.Unlock()
	}
}

// RecordBackgroundRead records a ReadChunk executed by enrichment/catchup across all active sessions.
func (sd *StreamDiagnostics) RecordBackgroundRead() {
	if sd == nil || !sd.enabled {
		return
	}
	sd.mu.RLock()
	defer sd.mu.RUnlock()
	for _, sess := range sd.sessions {
		sess.mu.Lock()
		sess.BackgroundReadsCount++
		sess.mu.Unlock()
	}
}

// GetRecentStats returns up to limit recent stats log lines.
func (sd *StreamDiagnostics) GetRecentStats(limit int) []string {
	if sd == nil || sd.ringBuffer == nil {
		return nil
	}
	return sd.ringBuffer.Get(limit)
}
