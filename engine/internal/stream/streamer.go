package stream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"

	"musix/engine/internal/config"
	"musix/engine/internal/logger"
	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

var logStream = logger.New("stream")

const oneMB = 1024 * 1024

// Streamer handles HTTP byte-range audio streaming from Telegram with multi-bot parallel prefetching.
type Streamer struct {
	client        telegram.Client
	prefetchCache *PrefetchCache
	bgCache       *BgChunkCache

	sessions      map[int64]*StreamSession
	sessionsMu    sync.RWMutex
	nextSessionID int64

	activePlaybackReads      int64
	lastPlaybackReadUnixNano int64
}

// NewStreamer creates a new audio streamer with parallel chunk prefetching.
func NewStreamer(client telegram.Client) *Streamer {
	return &Streamer{
		client:        client,
		prefetchCache: NewPrefetchCache(config.PrefetchCacheCapacity),
		bgCache:       NewBgChunkCache(config.BgCacheCapacity),
		sessions:      make(map[int64]*StreamSession),
	}
}

// OpenStream initializes a dedicated StreamSession for trackID, resolving metadata once.
func (s *Streamer) OpenStream(ctx context.Context, trackID string) (int64, error) {
	s.StartSession(trackID)
	handle := atomic.AddInt64(&s.nextSessionID, 1)

	session, err := NewStreamSession(ctx, handle, trackID, s)
	if err != nil {
		s.ReleaseSession(trackID)
		return 0, err
	}

	s.sessionsMu.Lock()
	s.sessions[handle] = session
	s.sessionsMu.Unlock()

	return handle, nil
}

// ReadStream reads up to length bytes at offset from the open session designated by handle.
func (s *Streamer) ReadStream(handle int64, offset int64, length int) ([]byte, error) {
	atomic.AddInt64(&s.activePlaybackReads, 1)
	defer atomic.AddInt64(&s.activePlaybackReads, -1)
	atomic.StoreInt64(&s.lastPlaybackReadUnixNano, time.Now().UnixNano())

	s.sessionsMu.RLock()
	session, ok := s.sessions[handle]
	s.sessionsMu.RUnlock()

	if !ok || session == nil {
		return nil, fmt.Errorf("invalid or closed stream handle: %d", handle)
	}

	return session.Read(offset, length)
}

// CloseStream terminates the stream session, cancelling in-flight work and releasing resources.
func (s *Streamer) CloseStream(handle int64) {
	s.sessionsMu.Lock()
	session, ok := s.sessions[handle]
	if ok {
		delete(s.sessions, handle)
	}
	s.sessionsMu.Unlock()

	if ok && session != nil {
		session.Close()
	}
}

// ActiveSessionCount returns the number of active streaming playback sessions.
func (s *Streamer) ActiveSessionCount() int {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return len(s.sessions)
}

// IsPlaybackActive returns true if playback chunks are actively in-flight or data was read within the last 3 seconds.
// This allows background workers to proceed when descriptors remain open for paused or preloaded tracks.
func (s *Streamer) IsPlaybackActive() bool {
	if atomic.LoadInt64(&s.activePlaybackReads) > 0 {
		return true
	}
	lastRead := atomic.LoadInt64(&s.lastPlaybackReadUnixNano)
	if lastRead > 0 && time.Since(time.Unix(0, lastRead)) < 3*time.Second {
		return true
	}
	return false
}

// StartSession initiates an active playback session for trackID.
func (s *Streamer) StartSession(trackID string) {}

// ReleaseSession terminates the active playback session for trackID.
func (s *Streamer) ReleaseSession(trackID string) {}

// GetTrackSize returns the total file size in bytes for a track, or an error if not found.
func (s *Streamer) GetTrackSize(ctx context.Context, trackID string) (int64, error) {
	track, err := s.client.ResolveTrack(ctx, trackID)
	if err != nil || track == nil {
		return 0, fmt.Errorf("track not found: %s", trackID)
	}
	if track.FileSize <= 0 {
		return 10 * 1024 * 1024, nil
	}
	return track.FileSize, nil
}

func (s *Streamer) downloadChunk(ctx context.Context, track *models.Track, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	if track != nil {
		ctx = models.WithTrackID(ctx, track.ID)
	}
	if pool, ok := s.client.(*telegram.Pool); ok && track != nil && track.ChatID != 0 && track.MsgID != 0 {
		return pool.DispatchChunkDownload(ctx, track.ChatID, track.MsgID, offset, limit)
	}
	return s.client.DownloadChunk(ctx, location, offset, limit)
}

func (s *Streamer) triggerPrefetch(track *models.Track, location *tg.InputDocumentFileLocation, offset int64, limit int) {
	if _, inCache := s.prefetchCache.Get(track.ID, offset, limit); inCache {
		return
	}
	go func() {
		prefetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if track != nil {
			prefetchCtx = models.WithTrackID(prefetchCtx, track.ID)
		}
		preChunk, preErr := s.downloadChunk(prefetchCtx, track, location, offset, limit)
		if preErr == nil && len(preChunk) > 0 {
			s.prefetchCache.Put(track.ID, offset, limit, preChunk)
		}
	}()
}

// ReadHeader reads only up to maxBytes for file header inspection without triggering
// prefetch and without polluting or evicting the playback cache.
func (s *Streamer) ReadHeader(ctx context.Context, trackID string, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	ctx = models.WithBackground(ctx)
	ctx = models.WithPriority(ctx, models.PriorityBackground)
	return s.ReadChunk(ctx, trackID, 0, maxBytes)
}

// ReadChunk reads up to limit bytes starting at offset for trackID directly from Telegram/cache.
func (s *Streamer) ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}

	readStart := time.Now()
	isBg := models.IsBackgroundContext(ctx)
	if !isBg {
		atomic.AddInt64(&s.activePlaybackReads, 1)
		defer atomic.AddInt64(&s.activePlaybackReads, -1)
		atomic.StoreInt64(&s.lastPlaybackReadUnixNano, time.Now().UnixNano())
	}
	ctx = models.WithTrackID(ctx, trackID)

	resolveStart := time.Now()
	track, err := s.client.ResolveTrack(ctx, trackID)
	resolveDur := time.Since(resolveStart)
	if err != nil || track == nil {
		return nil, fmt.Errorf("track not found: %s", trackID)
	}

	totalSize := track.FileSize
	if totalSize <= 0 {
		totalSize = 10 * 1024 * 1024
	}
	if offset >= totalSize {
		return nil, nil
	}

	var location *tg.InputDocumentFileLocation
	if track.FileID != "" {
		if decoded, err := telegram.DecodeFileID(track.FileID); err == nil {
			location = &tg.InputDocumentFileLocation{
				ID:            decoded.MediaID,
				AccessHash:    decoded.AccessHash,
				FileReference: decoded.FileReference,
			}
		}
	}
	if location == nil {
		var docID int64
		if strings.HasPrefix(track.ID, "doc_") {
			docID, _ = strconv.ParseInt(strings.TrimPrefix(track.ID, "doc_"), 10, 64)
		}
		if docID == 0 {
			docID = track.AccessHash
		}
		location = &tg.InputDocumentFileLocation{
			ID:            docID,
			AccessHash:    track.AccessHash,
			FileReference: track.FileReference,
		}
	}

	var result []byte
	currentOffset := offset
	bytesRemaining := int64(limit)
	if currentOffset+bytesRemaining > totalSize {
		bytesRemaining = totalSize - currentOffset
	}

	var totalDlDur time.Duration

	for bytesRemaining > 0 {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		if isBg {
			var chunk []byte
			var found bool
			if s.bgCache != nil {
				chunk, found = s.bgCache.GetRange(track.ID, currentOffset)
			}

			if !found {
				chunkOffset := (currentOffset / 4096) * 4096
				skipBytes := currentOffset - chunkOffset

				needed := bytesRemaining + skipBytes
				if needed > config.MaxBgBlockSize {
					needed = config.MaxBgBlockSize
				}

				// Align to 4096 bytes for MTProto limit
				alignedLimit := int(((needed + 4095) / 4096) * 4096)
				if alignedLimit > config.MaxBgBlockSize {
					alignedLimit = config.MaxBgBlockSize
				}
				if chunkOffset+int64(alignedLimit) > totalSize {
					alignedLimit = int(totalSize - chunkOffset)
				}

				dlStart := time.Now()
				var err error
				dlChunk, err := s.downloadChunk(ctx, track, location, chunkOffset, alignedLimit)
				totalDlDur += time.Since(dlStart)
				if err != nil {
					if len(result) > 0 {
						return result, nil
					}
					return nil, err
				}
				if len(dlChunk) == 0 {
					break
				}
				if s.bgCache != nil {
					s.bgCache.Put(track.ID, chunkOffset, dlChunk)
				}
				if skipBytes >= int64(len(dlChunk)) {
					break
				}
				chunk = dlChunk[skipBytes:]
			}

			toTake := bytesRemaining
			if toTake > int64(len(chunk)) {
				toTake = int64(len(chunk))
			}
			result = append(result, chunk[:toTake]...)
			bytesRemaining -= toTake
			currentOffset += toTake
			continue
		}

		// Interactive playback path
		chunkOffset := (currentOffset / 4096) * 4096
		skipBytes := currentOffset - chunkOffset

		bytesToBoundary := int64(oneMB) - (chunkOffset % int64(oneMB))
		currentLimit := config.DefaultChunkSize
		if int64(currentLimit) > bytesToBoundary {
			currentLimit = int(bytesToBoundary)
		}

		chunk, found := s.prefetchCache.Get(track.ID, chunkOffset, currentLimit)
		if !found {
			var err error
			chunk, err = s.downloadChunk(ctx, track, location, chunkOffset, currentLimit)
			if err != nil {
				if len(result) > 0 {
					return result, nil
				}
				return nil, err
			}
		}

		rawLen := len(chunk)
		if rawLen == 0 {
			break
		}

		// Asynchronous prefetch for next 2 chunks (1MB each) - only for interactive playback
		nextOffset := chunkOffset + int64(rawLen)
		if nextOffset < totalSize {
			nextBytesToBoundary := int64(oneMB) - (nextOffset % int64(oneMB))
			nextLimit := config.DefaultChunkSize
			if int64(nextLimit) > nextBytesToBoundary {
				nextLimit = int(nextBytesToBoundary)
			}
			s.triggerPrefetch(track, location, nextOffset, nextLimit)

			secondOffset := nextOffset + int64(nextLimit)
			if secondOffset < totalSize {
				secondBytesToBoundary := int64(oneMB) - (secondOffset % int64(oneMB))
				secondLimit := config.DefaultChunkSize
				if int64(secondLimit) > secondBytesToBoundary {
					secondLimit = int(secondBytesToBoundary)
				}
				s.triggerPrefetch(track, location, secondOffset, secondLimit)
			}
		}

		if skipBytes > 0 {
			if int64(rawLen) <= skipBytes {
				currentOffset += int64(rawLen)
				continue
			}
			chunk = chunk[skipBytes:]
			rawLen = len(chunk)
		}

		toTake := int64(rawLen)
		if toTake > bytesRemaining {
			toTake = bytesRemaining
		}
		result = append(result, chunk[:toTake]...)
		bytesRemaining -= toTake
		currentOffset += toTake

		if bytesRemaining <= 0 {
			break
		}
	}

	readDuration := time.Since(readStart)
	if isBg && readDuration > 3*time.Second {
		logStream.Warnf("[BG-READ-SLOW] track=%s offset=%d limit=%d total=%v resolve=%v dl=%v",
			trackID, offset, limit, readDuration, resolveDur, totalDlDur)
	}

	return result, nil
}


// ServeStream handles GET and HEAD /stream/:id.
func (s *Streamer) ServeStream(w http.ResponseWriter, r *http.Request, trackID string) {
	s.StartSession(trackID)
	defer s.ReleaseSession(trackID)

	ctx := r.Context()

	track, err := s.client.ResolveTrack(ctx, trackID)
	if err != nil || track == nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	totalSize := track.FileSize
	if totalSize <= 0 {
		totalSize = 10 * 1024 * 1024 // default fallback 10MB
	}

	mimeType := track.MimeType
	if mimeType == "" {
		mimeType = "audio/mpeg"
	}

	// 1. Parse Range Header
	rangeHeader := r.Header.Get("Range")
	var byteRange *ByteRange
	if rangeHeader != "" {
		byteRange, err = ParseRange(rangeHeader, totalSize)
		if errors.Is(err, ErrRangeNotSatisfiable) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	// 2. Set Response Headers
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", mimeType)

	fileName := track.FileName
	if fileName == "" {
		fileName = fmt.Sprintf("%s.mp3", track.ID)
	}
	fallback := strings.ReplaceAll(fileName, `"`, `_`)
	encoded := url.QueryEscape(fileName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"; filename*=UTF-8''%s`, fallback, encoded))

	status := http.StatusOK
	var contentLength int64 = totalSize

	if byteRange != nil {
		status = http.StatusPartialContent
		contentLength = byteRange.Length
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.Start, byteRange.End, totalSize))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))

	// If HEAD request, write headers and finish
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	w.WriteHeader(status)

	// 3. Stream chunks
	s.streamChunks(ctx, w, track, byteRange, totalSize)
}

func (s *Streamer) streamChunks(
	ctx context.Context,
	w http.ResponseWriter,
	track *models.Track,
	byteRange *ByteRange,
	totalSize int64,
) {
	var startOffset int64 = 0
	var bytesRemaining int64 = totalSize

	if byteRange != nil {
		startOffset = byteRange.Start
		bytesRemaining = byteRange.Length
	}

	var location *tg.InputDocumentFileLocation
	if track.FileID != "" {
		if decoded, err := telegram.DecodeFileID(track.FileID); err == nil {
			location = &tg.InputDocumentFileLocation{
				ID:            decoded.MediaID,
				AccessHash:    decoded.AccessHash,
				FileReference: decoded.FileReference,
			}
		}
	}
	if location == nil {
		var docID int64
		if strings.HasPrefix(track.ID, "doc_") {
			docID, _ = strconv.ParseInt(strings.TrimPrefix(track.ID, "doc_"), 10, 64)
		}
		if docID == 0 {
			docID = track.AccessHash
		}
		location = &tg.InputDocumentFileLocation{
			ID:            docID,
			AccessHash:    track.AccessHash,
			FileReference: track.FileReference,
		}
	}

	flusher, _ := w.(http.Flusher)

	// MTProto UploadGetFile requires offset divisible by 4096 (4KB)
	chunkOffset := (startOffset / 4096) * 4096
	skipBytes := startOffset - chunkOffset

	for bytesRemaining > 0 {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Calculate limit up to 1MB boundary
		bytesToBoundary := int64(oneMB) - (chunkOffset % int64(oneMB))
		currentLimit := config.DefaultChunkSize
		if int64(currentLimit) > bytesToBoundary {
			currentLimit = int(bytesToBoundary)
		}

		// 1. Check prefetch cache first
		chunk, found := s.prefetchCache.Get(track.ID, chunkOffset, currentLimit)
		if !found {
			var err error
			chunk, err = s.downloadChunk(ctx, track, location, chunkOffset, currentLimit)
			if err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return
				}
				return
			}
		}

		rawLen := len(chunk)
		if rawLen == 0 {
			break // EOF
		}

		// 2. Trigger asynchronous background prefetch for next 2 chunks (1MB each)
		nextOffset := chunkOffset + int64(rawLen)
		if nextOffset < totalSize {
			nextBytesToBoundary := int64(oneMB) - (nextOffset % int64(oneMB))
			nextLimit := config.DefaultChunkSize
			if int64(nextLimit) > nextBytesToBoundary {
				nextLimit = int(nextBytesToBoundary)
			}
			s.triggerPrefetch(track, location, nextOffset, nextLimit)

			secondOffset := nextOffset + int64(nextLimit)
			if secondOffset < totalSize {
				secondBytesToBoundary := int64(oneMB) - (secondOffset % int64(oneMB))
				secondLimit := config.DefaultChunkSize
				if int64(secondLimit) > secondBytesToBoundary {
					secondLimit = int(secondBytesToBoundary)
				}
				s.triggerPrefetch(track, location, secondOffset, secondLimit)
			}
		}

		// Handle sub-4KB unaligned start offset
		if skipBytes > 0 {
			if int64(rawLen) <= skipBytes {
				skipBytes -= int64(rawLen)
				chunkOffset += int64(rawLen)
				continue
			}
			chunk = chunk[skipBytes:]
			rawLen = len(chunk)
			skipBytes = 0
		}

		// Cap chunk to bytesRemaining
		if int64(rawLen) > bytesRemaining {
			chunk = chunk[:bytesRemaining]
			rawLen = len(chunk)
		}

		n, writeErr := w.Write(chunk)
		if writeErr != nil {
			return
		}

		if flusher != nil {
			flusher.Flush()
		}

		bytesRemaining -= int64(n)
		chunkOffset += int64(rawLen)
	}
}

