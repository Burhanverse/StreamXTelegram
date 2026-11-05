package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tommyo123/mtag"
)

// Helper to construct a FLAC fixture where the PICTURE block precedes the VORBIS_COMMENT block.
func buildFLACPictureBeforeComments(
	sampleRate int,
	channels int,
	totalSamples uint64,
	comments []string,
	pic pictureFixture,
) []byte {
	var buf bytes.Buffer
	buf.WriteString("fLaC")

	// 1. STREAMINFO (type 0, length 34, not last)
	streamInfo := make([]byte, 34)
	streamInfo[10] = byte(sampleRate >> 12)
	streamInfo[11] = byte(sampleRate >> 4)
	streamInfo[12] = byte((sampleRate&0x0F)<<4) | byte(((channels-1)&0x07)<<1)
	streamInfo[13] |= byte((totalSamples >> 32) & 0x0F)
	streamInfo[14] = byte(totalSamples >> 24)
	streamInfo[15] = byte(totalSamples >> 16)
	streamInfo[16] = byte(totalSamples >> 8)
	streamInfo[17] = byte(totalSamples)

	buf.WriteByte(0x00) // type 0, not last
	buf.Write([]byte{0, 0, 34})
	buf.Write(streamInfo)

	// 2. PICTURE block (type 6, not last)
	var pBuf bytes.Buffer
	_ = binary.Write(&pBuf, binary.BigEndian, pic.picType)
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(len(pic.mime)))
	pBuf.WriteString(pic.mime)
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(len(pic.desc)))
	pBuf.WriteString(pic.desc)
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(pic.width))
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(pic.height))
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(24)) // depth
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(0))  // colors
	_ = binary.Write(&pBuf, binary.BigEndian, uint32(len(pic.data)))
	pBuf.Write(pic.data)

	pData := pBuf.Bytes()
	buf.WriteByte(0x06) // type 6, not last
	buf.Write([]byte{byte(len(pData) >> 16), byte(len(pData) >> 8), byte(len(pData))})
	buf.Write(pData)

	// 3. VORBIS_COMMENT (type 4, isLast = true)
	var vcBuf bytes.Buffer
	vendor := "test-flac-writer"
	_ = binary.Write(&vcBuf, binary.LittleEndian, uint32(len(vendor)))
	vcBuf.WriteString(vendor)
	_ = binary.Write(&vcBuf, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		_ = binary.Write(&vcBuf, binary.LittleEndian, uint32(len(c)))
		vcBuf.WriteString(c)
	}
	vcData := vcBuf.Bytes()

	buf.WriteByte(0x84) // type 4, isLast = true
	buf.Write([]byte{byte(len(vcData) >> 16), byte(len(vcData) >> 8), byte(len(vcData))})
	buf.Write(vcData)

	return buf.Bytes()
}

// Requirement: Assert the count of underlying ReadChunk calls: <= 2 for a FLAC whose cover precedes the VORBIS_COMMENT block, with a 2MB cover.
func TestTask_FLACCoverPrecedingVorbisComment_Max2Reads(t *testing.T) {
	// Create a 2 MB valid JPEG payload
	twoMBCover := createValidJPEG(100, 100, 2*1024*1024)

	flacBytes := buildFLACPictureBeforeComments(
		44100, 2, 44100*120,
		[]string{"TITLE=FLAC Early Cover", "ARTIST=Cover First Artist"},
		pictureFixture{
			picType: 3, // CoverFront
			mime:    "image/jpeg",
			desc:    "Front Cover",
			width:   100,
			height:  100,
			data:    twoMBCover,
		},
	)

	mock := &mockChunkReader{data: flacBytes}
	ctx := context.Background()
	ra := newRangeReaderAt(ctx, "track_early_cover", mock, int64(len(flacBytes)))

	// Open with mtag using WithSkipPictures
	f, err := mtag.OpenSource(ra, int64(len(flacBytes)), mtag.WithReadOnly(), mtag.WithSkipV1(), mtag.WithSkipPictures())
	if err != nil {
		t.Fatalf("OpenSource failed: %v", err)
	}
	defer f.Close()

	if f.Title() != "FLAC Early Cover" {
		t.Errorf("Expected title %q, got %q", "FLAC Early Cover", f.Title())
	}
	if f.Artist() != "Cover First Artist" {
		t.Errorf("Expected artist %q, got %q", "Cover First Artist", f.Artist())
	}

	calls := ra.ReadCalls()
	if calls > 2 {
		t.Fatalf("Expected <= 2 ReadChunk calls when skipping 2MB cover preceding Vorbis comments, but got %d", calls)
	}
}

// Requirement: Test MP3 (ID3v2 + APIC) tag and artwork extraction
func TestTask_MP3_ID3v2_APIC(t *testing.T) {
	tmpDir := t.TempDir()
	coverPath := filepath.Join(tmpDir, "cover.jpg")
	mp3Path := filepath.Join(tmpDir, "test.mp3")
	artDir := filepath.Join(tmpDir, ArtworkDirName)

	// Generate 50x50 blue JPEG cover
	imgData := createValidJPEG(50, 50, 0)
	if err := os.WriteFile(coverPath, imgData, 0644); err != nil {
		t.Fatalf("Failed to write cover image: %v", err)
	}

	// Use ffmpeg to synthesize an MP3 with ID3v2 APIC cover frame
	cmd := exec.Command("ffmpeg",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo", "-t", "0.1",
		"-i", coverPath,
		"-map", "0:0", "-map", "1:0",
		"-c:a", "libmp3lame", "-c:v", "copy",
		"-id3v2_version", "3",
		"-metadata", "title=MP3 APIC Title",
		"-metadata", "artist=MP3 APIC Artist",
		"-metadata", "album=MP3 APIC Album",
		"-metadata", "date=2024",
		"-metadata:s:v", "title=Album cover",
		"-metadata:s:v", "comment=Cover (front)",
		"-frames:v", "1",
		mp3Path, "-y",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg failed to create MP3 test fixture: %v, output: %s", err, string(out))
	}

	mp3Bytes, err := os.ReadFile(mp3Path)
	if err != nil {
		t.Fatalf("Failed to read generated MP3: %v", err)
	}

	mock := &mockChunkReader{data: mp3Bytes}
	ctx := context.Background()

	meta, hasArt, err := ExtractEmbeddedMetadata(ctx, "track_mp3_test", mock, int64(len(mp3Bytes)), artDir)
	if err != nil {
		t.Fatalf("ExtractEmbeddedMetadata failed for MP3: %v", err)
	}

	if meta.Title != "MP3 APIC Title" {
		t.Errorf("Expected title %q, got %q", "MP3 APIC Title", meta.Title)
	}
	if meta.Artist != "MP3 APIC Artist" {
		t.Errorf("Expected artist %q, got %q", "MP3 APIC Artist", meta.Artist)
	}
	if meta.Album != "MP3 APIC Album" {
		t.Errorf("Expected album %q, got %q", "MP3 APIC Album", meta.Album)
	}
	if meta.Year != 2024 {
		t.Errorf("Expected year 2024, got %d", meta.Year)
	}
	if !hasArt {
		t.Errorf("Expected hasArt to be true for MP3 with APIC")
	}

	// Check files on disk
	thumbFile := filepath.Join(artDir, "track_mp3_test.jpg")
	if fi, err := os.Stat(thumbFile); err != nil || fi.Size() == 0 {
		t.Errorf("Expected downscaled thumbnail %s to exist with size > 0", thumbFile)
	}
}

// Requirement: Test M4A (ilst + covr) tag and artwork extraction
func TestTask_M4A_ILST_COVR(t *testing.T) {
	tmpDir := t.TempDir()
	coverPath := filepath.Join(tmpDir, "cover.jpg")
	m4aPath := filepath.Join(tmpDir, "test.m4a")
	artDir := filepath.Join(tmpDir, ArtworkDirName)

	imgData := createValidJPEG(50, 50, 0)
	if err := os.WriteFile(coverPath, imgData, 0644); err != nil {
		t.Fatalf("Failed to write cover image: %v", err)
	}

	// Use ffmpeg to synthesize an M4A with ilst covr
	cmd := exec.Command("ffmpeg",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo", "-t", "0.1",
		"-i", coverPath,
		"-map", "0:0", "-map", "1:0",
		"-c:a", "aac", "-c:v", "copy",
		"-disposition:v:0", "attached_pic",
		"-metadata", "title=M4A Covr Title",
		"-metadata", "artist=M4A Covr Artist",
		"-metadata", "album=M4A Covr Album",
		"-metadata", "date=2023",
		"-frames:v", "1",
		m4aPath, "-y",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg failed to create M4A test fixture: %v, output: %s", err, string(out))
	}

	m4aBytes, err := os.ReadFile(m4aPath)
	if err != nil {
		t.Fatalf("Failed to read generated M4A: %v", err)
	}

	mock := &mockChunkReader{data: m4aBytes}
	ctx := context.Background()

	meta, hasArt, err := ExtractEmbeddedMetadata(ctx, "track_m4a_test", mock, int64(len(m4aBytes)), artDir)
	if err != nil {
		t.Fatalf("ExtractEmbeddedMetadata failed for M4A: %v", err)
	}

	if meta.Title != "M4A Covr Title" {
		t.Errorf("Expected title %q, got %q", "M4A Covr Title", meta.Title)
	}
	if meta.Artist != "M4A Covr Artist" {
		t.Errorf("Expected artist %q, got %q", "M4A Covr Artist", meta.Artist)
	}
	if meta.Album != "M4A Covr Album" {
		t.Errorf("Expected album %q, got %q", "M4A Covr Album", meta.Album)
	}
	if meta.Year != 2023 {
		t.Errorf("Expected year 2023, got %d", meta.Year)
	}
	if !hasArt {
		t.Errorf("Expected hasArt to be true for M4A with covr")
	}

	thumbFile := filepath.Join(artDir, "track_m4a_test.jpg")
	if fi, err := os.Stat(thumbFile); err != nil || fi.Size() == 0 {
		t.Errorf("Expected downscaled thumbnail %s to exist with size > 0", thumbFile)
	}
}

// Requirement: Test Ogg Vorbis tag extraction
func TestTask_OGG_Vorbis(t *testing.T) {
	tmpDir := t.TempDir()
	oggPath := filepath.Join(tmpDir, "test.ogg")
	artDir := filepath.Join(tmpDir, ArtworkDirName)

	cmd := exec.Command("ffmpeg",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo", "-t", "0.1",
		"-c:a", "libvorbis",
		"-metadata", "title=OGG Vorbis Title",
		"-metadata", "artist=OGG Vorbis Artist",
		"-metadata", "album=OGG Vorbis Album",
		"-metadata", "date=2022",
		oggPath, "-y",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg failed to create OGG test fixture: %v, output: %s", err, string(out))
	}

	oggBytes, err := os.ReadFile(oggPath)
	if err != nil {
		t.Fatalf("Failed to read generated OGG: %v", err)
	}

	mock := &mockChunkReader{data: oggBytes}
	ctx := context.Background()

	meta, _, err := ExtractEmbeddedMetadata(ctx, "track_ogg_test", mock, int64(len(oggBytes)), artDir)
	if err != nil {
		t.Fatalf("ExtractEmbeddedMetadata failed for OGG: %v", err)
	}

	if meta.Title != "OGG Vorbis Title" {
		t.Errorf("Expected title %q, got %q", "OGG Vorbis Title", meta.Title)
	}
	if meta.Artist != "OGG Vorbis Artist" {
		t.Errorf("Expected artist %q, got %q", "OGG Vorbis Artist", meta.Artist)
	}
	if meta.Album != "OGG Vorbis Album" {
		t.Errorf("Expected album %q, got %q", "OGG Vorbis Album", meta.Album)
	}
}

// Requirement: Return sentinel ErrUnsupportedContainer for unknown formats
func TestTask_UnsupportedContainer(t *testing.T) {
	randomBytes := []byte("This is not an audio stream at all, just arbitrary text data.")
	mock := &mockChunkReader{data: randomBytes}
	ctx := context.Background()

	_, _, err := ExtractEmbeddedMetadata(ctx, "track_random", mock, int64(len(randomBytes)), "")
	if err == nil {
		t.Fatalf("Expected error for non-audio data, got nil")
	}
	if !errors.Is(err, ErrUnsupportedContainer) {
		t.Fatalf("Expected errors.Is(err, ErrUnsupportedContainer), got: %v", err)
	}
}

// Requirement: Keep ExtractFLACMetadata as a thin deprecated wrapper so existing tests compile
func TestTask_ExtractFLACMetadata_DeprecatedWrapper(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	flacBytes := buildFLACFixture(
		44100, 2, 44100*60,
		[]string{"TITLE=Wrapper Test", "ARTIST=Wrapper Artist"},
		nil,
		0,
	)

	mock := &mockChunkReader{data: flacBytes}
	meta, hasArt, err := ExtractFLACMetadata(context.Background(), "track_wrapper", mock, artDir)
	if err != nil {
		t.Fatalf("ExtractFLACMetadata failed: %v", err)
	}
	if meta.Title != "Wrapper Test" || meta.Artist != "Wrapper Artist" {
		t.Errorf("Metadata mismatch: %+v", meta)
	}
	if hasArt {
		t.Errorf("Expected hasArt to be false when no picture is present")
	}
}

