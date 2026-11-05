package metadata

import (
	"bytes"
	"context"
	"io"
	"testing"
)

type mockChunkReader struct {
	data      []byte
	callCount int
}

func (m *mockChunkReader) ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error) {
	m.callCount++
	if offset >= int64(len(m.data)) {
		return []byte{}, nil
	}
	end := offset + int64(limit)
	if end > int64(len(m.data)) {
		end = int64(len(m.data))
	}
	res := make([]byte, end-offset)
	copy(res, m.data[offset:end])
	return res, nil
}

func TestRangeReaderAt_CachingAndEOF(t *testing.T) {
	// 600 KB total data
	const totalSize = 600 * 1024
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i % 251)
	}

	mock := &mockChunkReader{data: data}
	ctx := context.Background()
	ra := newRangeReaderAt(ctx, "test_track", mock, totalSize)

	// 1. Read first 64 KB (offset 0..65536) -> triggers 1 block download (0..256KB)
	buf1 := make([]byte, 64*1024)
	n, err := ra.ReadAt(buf1, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 64*1024 || !bytes.Equal(buf1, data[:64*1024]) {
		t.Fatalf("Data mismatch on first read")
	}
	if ra.ReadCalls() != 1 {
		t.Fatalf("Expected 1 underlying read call, got %d", ra.ReadCalls())
	}

	// 2. Read next 128 KB (offset 64KB..192KB) inside same 256KB block -> cache hit!
	buf2 := make([]byte, 128*1024)
	n, err = ra.ReadAt(buf2, 64*1024)
	if err != nil {
		t.Fatalf("ReadAt hit failed: %v", err)
	}
	if n != 128*1024 || !bytes.Equal(buf2, data[64*1024:192*1024]) {
		t.Fatalf("Data mismatch on second read")
	}
	if ra.ReadCalls() != 1 {
		t.Fatalf("Expected still 1 underlying read call, got %d", ra.ReadCalls())
	}

	// 3. Read spanning beyond EOF: at offset 550KB, read 100KB (file ends at 600KB)
	buf3 := make([]byte, 100*1024)
	n, err = ra.ReadAt(buf3, 550*1024)
	if err != io.EOF {
		t.Fatalf("Expected io.EOF, got %v", err)
	}
	if n != 50*1024 {
		t.Fatalf("Expected 50 KB read before EOF, got %d", n)
	}
	if !bytes.Equal(buf3[:n], data[550*1024:]) {
		t.Fatalf("Data mismatch on tail read")
	}

	// 4. Read starting at or after EOF -> immediate 0, io.EOF
	buf4 := make([]byte, 10)
	n, err = ra.ReadAt(buf4, totalSize)
	if err != io.EOF || n != 0 {
		t.Fatalf("Expected 0, io.EOF at totalSize, got %d, %v", n, err)
	}
}

