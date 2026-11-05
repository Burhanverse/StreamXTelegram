package stream

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"

	"musix/engine/internal/config"
	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

// StreamSession manages an open playback stream with singleflight deduplication,
// single track/location resolution, adaptive chunk sizing, and seek prefetch cancellation.
type StreamSession struct {
	ID          int64
	TrackID     string
	Track       *models.Track
	Location    *tg.InputDocumentFileLocation
	TotalSize   int64
	DurationSec int
	BitrateBps  int64

	streamer *Streamer
	ctx      context.Context
	cancel   context.CancelFunc

	flight singleflight.Group

	mu              sync.Mutex
	lastReadOffset  int64
	prefetchCancels map[string]context.CancelFunc
	closed          bool
}

// NewStreamSession creates and initializes a StreamSession for trackID.
func NewStreamSession(parentCtx context.Context, id int64, trackID string, streamer *Streamer) (*StreamSession, error) {
	track, err := streamer.client.ResolveTrack(parentCtx, trackID)
	if err != nil || track == nil {
		return nil, fmt.Errorf("track not found: %s", trackID)
	}

	totalSize := track.FileSize
	if totalSize <= 0 {
		totalSize = 10 * 1024 * 1024
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

	durationSec := track.DurationSec
	var bitrate int64 = config.DefaultBitrateBps
	if durationSec > 0 && totalSize > 0 {
		bitrate = totalSize / int64(durationSec)
		if bitrate < 8000 {
			bitrate = 8000
		} else if bitrate > 250000 {
			bitrate = 250000
		}
	}

	ctx, cancel := context.WithCancel(parentCtx)
	ctx = models.WithTrackID(ctx, trackID)

	s := &StreamSession{
		ID:              id,
		TrackID:         trackID,
		Track:           track,
		Location:        location,
		TotalSize:       totalSize,
		DurationSec:     durationSec,
		BitrateBps:      bitrate,
		streamer:        streamer,
		ctx:             ctx,
		cancel:          cancel,
		prefetchCancels: make(map[string]context.CancelFunc),
	}

	return s, nil
}

// Read reads up to length bytes at offset from the session's sliding chunk window.
func (s *StreamSession) Read(offset int64, length int) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}

	if offset >= s.TotalSize {
		return nil, nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, context.Canceled
	}

	// Detect seek jump: if reading more than 512 KB away from previous offset, cancel obsolete prefetches
	if s.lastReadOffset > 0 && (offset < s.lastReadOffset-256*1024 || offset > s.lastReadOffset+2*1024*1024) {
		s.cancelPrefetchesOutsideWindowLocked(offset)
	}
	s.lastReadOffset = offset + int64(length)
	s.mu.Unlock()

	var result []byte
	currentOffset := offset
	bytesRemaining := int64(length)
	if currentOffset+bytesRemaining > s.TotalSize {
		bytesRemaining = s.TotalSize - currentOffset
	}

	for bytesRemaining > 0 {
		select {
		case <-s.ctx.Done():
			return result, s.ctx.Err()
		default:
		}

		chunkOffset, chunkLimit := s.calculateChunkGeometry(currentOffset)
		skipBytes := currentOffset - chunkOffset

		chunk, err := s.fetchChunkWithSingleflight(chunkOffset, chunkLimit)
		if err != nil {
			if len(result) > 0 {
				return result, nil
			}
			return nil, err
		}

		rawLen := len(chunk)
		if rawLen == 0 {
			break
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

	// Trigger read-ahead prefetching for upcoming chunks within the sliding window
	s.triggerSlidingWindowPrefetch(offset + int64(length))

	return result, nil
}

// calculateChunkGeometry determines the aligned offset and limit for a given byte offset.
// For the beginning of the file (offset < 256 KB), an adaptive 256 KB chunk is used for fast TTFB.
func (s *StreamSession) calculateChunkGeometry(offset int64) (int64, int) {
	const oneMB int64 = 1024 * 1024

	// Adaptive initial chunk: 0..256 KB
	if offset < config.InitialSmallChunkSize {
		return 0, config.InitialSmallChunkSize
	}
	// Remainder of first megabyte: 256 KB..1 MB
	if offset < oneMB {
		return config.InitialSmallChunkSize, int(oneMB - config.InitialSmallChunkSize)
	}

	// Normal 1 MB aligned chunk
	alignedOffset := (offset / 4096) * 4096
	bytesToBoundary := oneMB - (alignedOffset % oneMB)
	limit := config.FullChunkSize
	if int64(limit) > bytesToBoundary {
		limit = int(bytesToBoundary)
	}
	return alignedOffset, limit
}

// fetchChunkWithSingleflight gets a chunk from cache or downloads it via singleflight deduplication.
func (s *StreamSession) fetchChunkWithSingleflight(chunkOffset int64, chunkLimit int) ([]byte, error) {
	// 1. Check cache (zero-copy immutable slice)
	if data, found := s.streamer.prefetchCache.Get(s.TrackID, chunkOffset, chunkLimit); found {
		return data, nil
	}

	// 2. Singleflight deduplication per (trackID, chunkOffset, chunkLimit)
	key := fmt.Sprintf("%s:%d:%d", s.TrackID, chunkOffset, chunkLimit)
	val, err, _ := s.flight.Do(key, func() (any, error) {
		// Double-check cache in case another flight just finished
		if data, found := s.streamer.prefetchCache.Get(s.TrackID, chunkOffset, chunkLimit); found {
			return data, nil
		}

		ctx := models.WithPriority(s.ctx, models.PriorityPlayback)
		chunk, dErr := s.streamer.downloadChunk(ctx, s.Track, s.Location, chunkOffset, chunkLimit)
		if dErr != nil {
			return nil, dErr
		}
		if len(chunk) > 0 {
			s.streamer.prefetchCache.Put(s.TrackID, chunkOffset, chunkLimit, chunk)
		}
		return chunk, nil
	})

	if err != nil {
		return nil, err
	}
	return val.([]byte), nil
}

// triggerSlidingWindowPrefetch queues prefetches for chunks within the target seconds window.
func (s *StreamSession) triggerSlidingWindowPrefetch(fromOffset int64) {
	windowBytes := s.BitrateBps * config.DefaultReadAheadSec
	if windowBytes < config.MinReadAheadBytes {
		windowBytes = config.MinReadAheadBytes
	} else if windowBytes > config.MaxReadAheadBytes {
		windowBytes = config.MaxReadAheadBytes
	}

	endOffset := fromOffset + windowBytes
	if endOffset > s.TotalSize {
		endOffset = s.TotalSize
	}

	nextOffset := fromOffset
	for nextOffset < endOffset {
		chunkOffset, chunkLimit := s.calculateChunkGeometry(nextOffset)
		nextOffset = chunkOffset + int64(chunkLimit)

		if s.streamer.prefetchCache.Has(s.TrackID, chunkOffset, chunkLimit) {
			continue
		}

		key := fmt.Sprintf("%s:%d:%d", s.TrackID, chunkOffset, chunkLimit)

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if _, inFlight := s.prefetchCancels[key]; inFlight {
			s.mu.Unlock()
			continue
		}

		prefetchCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
		prefetchCtx = models.WithPriority(prefetchCtx, models.PriorityPrefetch)
		prefetchCtx = models.WithTrackID(prefetchCtx, s.TrackID)
		s.prefetchCancels[key] = cancel
		s.mu.Unlock()

		go func(pCtx context.Context, pKey string, cOffset int64, cLimit int) {
			defer func() {
				s.mu.Lock()
				delete(s.prefetchCancels, pKey)
				s.mu.Unlock()
			}()

			_, _, _ = s.flight.Do(pKey, func() (any, error) {
				if s.streamer.prefetchCache.Has(s.TrackID, cOffset, cLimit) {
					return nil, nil
				}
				chunk, pErr := s.streamer.downloadChunk(pCtx, s.Track, s.Location, cOffset, cLimit)
				if pErr == nil && len(chunk) > 0 {
					s.streamer.prefetchCache.Put(s.TrackID, cOffset, cLimit, chunk)
				}
				return chunk, pErr
			})
		}(prefetchCtx, key, chunkOffset, chunkLimit)
	}
}

// cancelPrefetchesOutsideWindowLocked cancels background prefetches no longer within window on seek.
func (s *StreamSession) cancelPrefetchesOutsideWindowLocked(newOffset int64) {
	windowBytes := s.BitrateBps * config.DefaultReadAheadSec
	if windowBytes < config.MinReadAheadBytes {
		windowBytes = config.MinReadAheadBytes
	}
	windowEnd := newOffset + windowBytes

	for key, cancel := range s.prefetchCancels {
		parts := strings.Split(key, ":")
		if len(parts) >= 2 {
			cOffset, _ := strconv.ParseInt(parts[1], 10, 64)
			if cOffset < newOffset || cOffset > windowEnd {
				cancel()
				delete(s.prefetchCancels, key)
			}
		}
	}
}

// Close closes the session, cancels all in-flight prefetches and stops all session goroutines.
func (s *StreamSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true

	for key, cancel := range s.prefetchCancels {
		cancel()
		delete(s.prefetchCancels, key)
	}
	s.cancel()
	s.mu.Unlock()

	s.streamer.ReleaseSession(s.TrackID)
}
