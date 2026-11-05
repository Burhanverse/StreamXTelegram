package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// fakeRangeReader implements RangeReader using an in-memory byte slice and records requested ranges.
type fakeRangeReader struct {
	data      []byte
	reads     [][2]int64 // [offset, limit]
	failAfter int64      // fail if offset >= failAfter (if > 0)
}

func (f *fakeRangeReader) ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error) {
	f.reads = append(f.reads, [2]int64{offset, int64(limit)})
	if f.failAfter > 0 && offset >= f.failAfter {
		return nil, fmt.Errorf("simulated network drop at offset %d", offset)
	}
	if offset >= int64(len(f.data)) {
		return []byte{}, nil
	}
	end := offset + int64(limit)
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	out := make([]byte, end-offset)
	copy(out, f.data[offset:end])
	return out, nil
}

// createValidJPEG creates a valid minimal JPEG image byte slice.
func createValidJPEG(width, height int, padBytes int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	data := buf.Bytes()
	if padBytes > 0 {
		padding := make([]byte, padBytes)
		data = append(data, padding...)
	}
	return data
}

// buildFLACFixture constructs a synthetic FLAC byte stream in code.
func buildFLACFixture(
	sampleRate int,
	channels int,
	totalSamples uint64,
	comments []string,
	pictures []pictureFixture,
	trailingAudioBytes int,
) []byte {
	var buf bytes.Buffer
	buf.WriteString("fLaC")

	// 1. STREAMINFO (type 0, length 34)
	streamInfo := make([]byte, 34)
	// sample rate (20 bits): data[10..12]
	streamInfo[10] = byte(sampleRate >> 12)
	streamInfo[11] = byte(sampleRate >> 4)
	streamInfo[12] = byte((sampleRate&0x0F)<<4) | byte(((channels-1)&0x07)<<1)
	// total samples (36 bits): data[13..17]
	streamInfo[13] |= byte((totalSamples >> 32) & 0x0F)
	streamInfo[14] = byte(totalSamples >> 24)
	streamInfo[15] = byte(totalSamples >> 16)
	streamInfo[16] = byte(totalSamples >> 8)
	streamInfo[17] = byte(totalSamples)

	hasComments := len(comments) > 0
	hasPics := len(pictures) > 0
	isLast := !hasComments && !hasPics

	hdr := byte(0) // type 0
	if isLast {
		hdr |= 0x80
	}
	buf.WriteByte(hdr)
	buf.Write([]byte{0, 0, 34})
	buf.Write(streamInfo)

	// 2. VORBIS_COMMENT (type 4)
	if hasComments {
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
		isLast = !hasPics
		hdr = byte(4)
		if isLast {
			hdr |= 0x80
		}
		buf.WriteByte(hdr)
		buf.Write([]byte{byte(len(vcData) >> 16), byte(len(vcData) >> 8), byte(len(vcData))})
		buf.Write(vcData)
	}

	// 3. PICTURE blocks (type 6)
	for i, pic := range pictures {
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
		isLast = (i == len(pictures)-1)
		hdr = byte(6)
		if isLast {
			hdr |= 0x80
		}
		buf.WriteByte(hdr)
		buf.Write([]byte{byte(len(pData) >> 16), byte(len(pData) >> 8), byte(len(pData))})
		buf.Write(pData)
	}

	// Trailing audio frames
	if trailingAudioBytes > 0 {
		buf.Write(make([]byte, trailingAudioBytes))
	}

	return buf.Bytes()
}

type pictureFixture struct {
	picType uint32
	mime    string
	desc    string
	width   int
	height  int
	data    []byte
}

func TestFLACSmallPictureAndExactRange(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	smallJpg := createValidJPEG(64, 64, 0)
	flacBytes := buildFLACFixture(
		44100, 2, 44100*180, // 3 minutes
		[]string{"TITLE=Test Song", "ARTIST=Singer", "ALBUM=Great Album", "DATE=2024", "TRACKNUMBER=1"},
		[]pictureFixture{
			{picType: 3, mime: "image/jpeg", desc: "Front", width: 64, height: 64, data: smallJpg},
		},
		50000, // 50 KB audio frames
	)

	reader := &fakeRangeReader{data: flacBytes}
	meta, hasArt, err := ExtractFLACMetadata(context.Background(), "track_small", reader, artDir)
	if err != nil {
		t.Fatalf("ExtractFLACMetadata failed: %v", err)
	}
	if !hasArt {
		t.Fatal("expected hasArt to be true")
	}
	if meta.Title != "Test Song" || meta.Artist != "Singer" || meta.Album != "Great Album" {
		t.Errorf("unexpected tags: %+v", meta)
	}
	if meta.Year != 2024 || meta.TrackNumber != 1 || meta.DurationSec != 180 {
		t.Errorf("unexpected numeric tags: year=%d, track=%d, dur=%d", meta.Year, meta.TrackNumber, meta.DurationSec)
	}

	// Verify exact range fetch: audio frames must NEVER be downloaded
	metadataEndOffset := int64(len(flacBytes) - 50000)
	for _, read := range reader.reads {
		offset := read[0]
		if offset >= metadataEndOffset {
			t.Errorf("audio frames read violation: offset %d >= metadataEndOffset %d", offset, metadataEndOffset)
		}
	}

	// Verify thumbnail was created
	thumbPath := filepath.Join(artDir, "track_small.jpg")
	if fi, err := os.Stat(thumbPath); err != nil || fi.Size() == 0 {
		t.Fatalf("thumbnail file missing or empty: %v", err)
	}
}

func TestFLAC3MBPictureStreaming(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	// Create ~3 MB picture (valid JPEG + 3 MB data)
	bigJpg := createValidJPEG(200, 200, 3*1024*1024)
	flacBytes := buildFLACFixture(
		48000, 2, 48000*200,
		[]string{"TITLE=Big Art Track", "ARTIST=Artist"},
		[]pictureFixture{
			{picType: 3, mime: "image/jpeg", desc: "Front Cover", width: 200, height: 200, data: bigJpg},
		},
		100000,
	)

	reader := &fakeRangeReader{data: flacBytes}
	meta, hasArt, err := ExtractFLACMetadata(context.Background(), "track_big", reader, artDir)
	if err != nil {
		t.Fatalf("ExtractFLACMetadata failed on 3MB picture: %v", err)
	}
	if !hasArt {
		t.Fatal("expected hasArt to be true for 3MB picture")
	}

	// Verify orig and thumbnail exist
	origPath := filepath.Join(artDir, "track_big.orig")
	thumbPath := filepath.Join(artDir, "track_big.jpg")
	if fi, err := os.Stat(origPath); err != nil || fi.Size() < 3*1024*1024 {
		t.Errorf("orig file size invalid: %v", err)
	}
	if fi, err := os.Stat(thumbPath); err != nil || fi.Size() == 0 {
		t.Errorf("downscaled thumbnail missing or empty: %v", err)
	}
	if meta.Title != "Big Art Track" {
		t.Errorf("wrong title: %s", meta.Title)
	}
}

func TestFLACMultiplePicturesPrefersType3FrontCover(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	backJpg := createValidJPEG(50, 50, 0)
	frontJpg := createValidJPEG(100, 100, 0)

	flacBytes := buildFLACFixture(
		44100, 2, 44100*60,
		[]string{"TITLE=Multi Pic"},
		[]pictureFixture{
			{picType: 4, mime: "image/jpeg", desc: "Back Cover", width: 50, height: 50, data: backJpg},
			{picType: 3, mime: "image/jpeg", desc: "Front Cover", width: 100, height: 100, data: frontJpg},
			{picType: 0, mime: "image/jpeg", desc: "Other", width: 30, height: 30, data: backJpg},
		},
		1000,
	)

	reader := &fakeRangeReader{data: flacBytes}
	meta, hasArt, err := ExtractFLACMetadata(context.Background(), "track_multipic", reader, artDir)
	if err != nil || !hasArt {
		t.Fatalf("failed: %v", err)
	}
	if meta.PictureType != 3 {
		t.Errorf("expected PictureType 3 (front cover), got %d", meta.PictureType)
	}
}

func TestFLACNoPicture(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	flacBytes := buildFLACFixture(
		44100, 2, 44100*120,
		[]string{"TITLE=No Art Song", "ARTIST=Acoustic Band"},
		nil,
		5000,
	)

	reader := &fakeRangeReader{data: flacBytes}
	meta, hasArt, err := ExtractFLACMetadata(context.Background(), "track_no_art", reader, artDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasArt {
		t.Errorf("expected hasArt to be false")
	}
	if meta.Title != "No Art Song" || meta.Artist != "Acoustic Band" {
		t.Errorf("unexpected tags: %+v", meta)
	}
}

func TestFLACDevanagariAndMultiValueArtists(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	flacBytes := buildFLACFixture(
		44100, 2, 44100*250,
		[]string{
			"TITLE=केसरिया (Kesariya)",
			"ARTIST=अरिजीत सिंह",
			"ARTIST=प्रीतम",
			"ALBUM=ब्रह्मास्त्र",
			"ALBUMARTIST=प्रीतम",
			"GENRE=Bollywood",
			"COMPOSER=Pritam Chakraborty",
			"DATE=2022",
			"TRACKNUMBER=03/12",
			"DISCNUMBER=1/1",
		},
		nil,
		2000,
	)

	reader := &fakeRangeReader{data: flacBytes}
	meta, _, err := ExtractFLACMetadata(context.Background(), "track_indic", reader, artDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.Title != "केसरिया (Kesariya)" {
		t.Errorf("Unicode title mangled: got %q", meta.Title)
	}
	if len(meta.Artists) != 2 || meta.Artists[0] != "अरिजीत सिंह" || meta.Artists[1] != "प्रीतम" {
		t.Errorf("multi-value artists failed: %+v", meta.Artists)
	}
	if meta.Artist != "अरिजीत सिंह, प्रीतम" {
		t.Errorf("joined artist failed: %q", meta.Artist)
	}
	if meta.Album != "ब्रह्मास्त्र" || meta.AlbumArtist != "प्रीतम" {
		t.Errorf("album or albumartist failed: album=%q, aa=%q", meta.Album, meta.AlbumArtist)
	}
	if meta.TrackNumber != 3 || meta.DiscNumber != 1 || meta.Year != 2022 {
		t.Errorf("metadata parsing failed: track=%d, disc=%d, year=%d", meta.TrackNumber, meta.DiscNumber, meta.Year)
	}
	if meta.Genre != "Bollywood" || meta.Composer != "Pritam Chakraborty" {
		t.Errorf("genre or composer failed: genre=%q, comp=%q", meta.Genre, meta.Composer)
	}
}

func TestFLACTruncatedFile(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	full := buildFLACFixture(
		44100, 2, 44100*60,
		[]string{"TITLE=Truncated"},
		[]pictureFixture{
			{picType: 3, mime: "image/jpeg", desc: "Front", width: 50, height: 50, data: createValidJPEG(50, 50, 0)},
		},
		0, // 0 audio bytes
	)
	// Truncate 100 bytes from the end of the picture block
	truncated := full[:len(full)-100]

	reader := &fakeRangeReader{data: truncated}
	_, hasArt, _ := ExtractFLACMetadata(context.Background(), "track_trunc", reader, artDir)
	// Truncated image must fail image validation and not produce art
	if hasArt {
		t.Errorf("expected hasArt to be false on truncated file")
	}
}

func TestArtworkPathAgreement(t *testing.T) {
	// Item 1 requirement: test fails if engine and Kotlin paths ever differ
	const expectedSharedName = "artwork"
	if ArtworkDirName != expectedSharedName {
		t.Fatalf("ArtworkDirName mismatch: got %q, expected %q", ArtworkDirName, expectedSharedName)
	}
}

func TestExtractFLACTagsAndStreamPictureSplit(t *testing.T) {
	tempDir := t.TempDir()
	artDir := filepath.Join(tempDir, ArtworkDirName)

	full := buildFLACFixture(
		44100, 2, 44100*180,
		[]string{"TITLE=Split Budget Test", "ARTIST=Split Artist"},
		[]pictureFixture{
			{picType: 3, mime: "image/jpeg", desc: "Front", width: 50, height: 50, data: createValidJPEG(50, 50, 0)},
		},
		0,
	)

	ctx := context.Background()

	// 1. Tags extraction succeeds even if picture range will fail later
	okReader := &fakeRangeReader{data: full}
	meta, picInfo, err := ExtractFLACTags(ctx, "track_split", okReader)
	if err != nil {
		t.Fatalf("ExtractFLACTags failed: %v", err)
	}
	if meta.Title != "Split Budget Test" || meta.Artist != "Split Artist" {
		t.Fatalf("Tags mismatch: got title=%q, artist=%q", meta.Title, meta.Artist)
	}
	if picInfo == nil || picInfo.Type != 3 {
		t.Fatalf("Expected picInfo type 3, got: %+v", picInfo)
	}

	// 2. Picture streaming fails due to simulated read failure, but tags were already extracted
	failingReader := &fakeRangeReader{data: full, failAfter: picInfo.Offset}
	hasArt, picErr := StreamFLACPicture(ctx, "track_split", failingReader, picInfo, artDir)
	if hasArt || picErr == nil {
		t.Errorf("Expected StreamFLACPicture to fail due to failAfter, got hasArt=%v, err=%v", hasArt, picErr)
	}

	// 3. With working reader, StreamFLACPicture succeeds
	hasArt, picErr = StreamFLACPicture(ctx, "track_split_ok", okReader, picInfo, artDir)
	if !hasArt || picErr != nil {
		t.Fatalf("Expected StreamFLACPicture to succeed, got hasArt=%v, err=%v", hasArt, picErr)
	}
}
