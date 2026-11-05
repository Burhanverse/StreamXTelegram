package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

const (
	readerAtBlockSize = 256 * 1024 // 256 KB block alignment
	readerAtMaxBlocks = 4          // Tiny LRU (4 x 256 KB = 1 MB max memory)
)

type rangeReaderAtBlock struct {
	offset int64
	data   []byte
}

// rangeReaderAt implements io.ReaderAt and RangeReader over an underlying RangeReader.
// It uses 256KB-aligned block reads with a 4-block LRU cache and singleflight per block.
type rangeReaderAt struct {
	ctx       context.Context
	trackID   string
	reader    RangeReader
	size      int64
	sf        singleflight.Group
	readCount int64 // atomic counter for underlying ReadChunk invocations
	mu        sync.Mutex
	cache     []*rangeReaderAtBlock
}

func newRangeReaderAt(ctx context.Context, trackID string, reader RangeReader, size int64) *rangeReaderAt {
	return &rangeReaderAt{
		ctx:     ctx,
		trackID: trackID,
		reader:  reader,
		size:    size,
		cache:   make([]*rangeReaderAtBlock, 0, readerAtMaxBlocks),
	}
}

// ReadCalls returns the total number of underlying ReadChunk calls issued.
func (r *rangeReaderAt) ReadCalls() int64 {
	return atomic.LoadInt64(&r.readCount)
}

// ReadChunk implements RangeReader over rangeReaderAt, benefiting from the block cache.
func (r *rangeReaderAt) ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error) {
	if limit <= 0 {
		return []byte{}, nil
	}
	buf := make([]byte, limit)
	n, err := r.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// ReadAt reads len(p) bytes into p starting at offset off in the underlying stream.
// It honors the context stored in the struct, returns io.EOF at or beyond file size,
// and routes chunk downloads through 256KB-aligned cached blocks.
func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.size > 0 && off >= r.size {
		return 0, io.EOF
	}

	totalToRead := len(p)
	reachedEOF := false
	if r.size > 0 && off+int64(totalToRead) >= r.size {
		totalToRead = int(r.size - off)
		reachedEOF = true
	}

	n := 0
	currOff := off

	for n < totalToRead {
		select {
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		default:
		}

		blockOffset := (currOff / readerAtBlockSize) * readerAtBlockSize
		blockData, err := r.getBlock(blockOffset)
		if err != nil {
			return n, err
		}

		offsetInBlock := currOff - blockOffset
		if offsetInBlock >= int64(len(blockData)) {
			// No more bytes available from underlying reader
			reachedEOF = true
			break
		}

		avail := int64(len(blockData)) - offsetInBlock
		want := int64(totalToRead - n)
		toCopy := want
		if toCopy > avail {
			toCopy = avail
		}

		copy(p[n:n+int(toCopy)], blockData[offsetInBlock:offsetInBlock+toCopy])
		n += int(toCopy)
		currOff += toCopy

		if int64(len(blockData)) < readerAtBlockSize && (r.size <= 0 || currOff >= r.size) {
			reachedEOF = true
			break
		}
	}

	if reachedEOF && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *rangeReaderAt) getBlock(blockOffset int64) ([]byte, error) {
	// 1. Fast LRU cache lookup
	r.mu.Lock()
	for i, b := range r.cache {
		if b.offset == blockOffset {
			if i < len(r.cache)-1 {
				r.cache = append(append(r.cache[:i], r.cache[i+1:]...), b)
			}
			data := b.data
			r.mu.Unlock()
			return data, nil
		}
	}
	r.mu.Unlock()

	// 2. Fetch block using singleflight per block offset
	key := fmt.Sprintf("%d", blockOffset)
	v, err, _ := r.sf.Do(key, func() (interface{}, error) {
		// Recheck cache inside singleflight callback
		r.mu.Lock()
		for i, b := range r.cache {
			if b.offset == blockOffset {
				if i < len(r.cache)-1 {
					r.cache = append(append(r.cache[:i], r.cache[i+1:]...), b)
				}
				data := b.data
				r.mu.Unlock()
				return data, nil
			}
		}
		r.mu.Unlock()

		limit := readerAtBlockSize
		if r.size > 0 && blockOffset+int64(limit) > r.size {
			limit = int(r.size - blockOffset)
		}
		if limit <= 0 {
			return []byte{}, nil
		}

		atomic.AddInt64(&r.readCount, 1)
		data, err := r.reader.ReadChunk(r.ctx, r.trackID, blockOffset, limit)
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		if len(r.cache) >= readerAtMaxBlocks {
			r.cache = r.cache[1:]
		}
		r.cache = append(r.cache, &rangeReaderAtBlock{
			offset: blockOffset,
			data:   data,
		})
		r.mu.Unlock()

		return data, nil
	})

	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

