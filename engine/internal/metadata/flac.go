package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/image/draw"
)

const (
	// MaxFLACMetadataSize caps metadata scanning to 12 MB to prevent memory exhaustion
	MaxFLACMetadataSize = 12 * 1024 * 1024
	// InitialFLACScanChunk is the size of the initial header chunk
	InitialFLACScanChunk = 64 * 1024
	// ArtworkDirName is the shared single directory name for cover artwork
	ArtworkDirName = "artwork"
	// MaxThumbnailDimension is the bounding box for downscaled list thumbnails
	MaxThumbnailDimension = 512
)

// RangeReader is an interface for reading arbitrary byte ranges of an audio track.
type RangeReader interface {
	ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error)
}

// FLACMetadata contains all authoritative tags and picture coordinates extracted from a FLAC file.
type FLACMetadata struct {
	Title        string
	Artists      []string
	Artist       string
	Album        string
	AlbumArtist  string
	Year         int
	TrackNumber  int
	DiscNumber   int
	Genre        string
	Composer     string
	DurationSec  int
	SampleRate   int
	Channels     int
	TotalSamples uint64

	// Picture block info
	HasPicture    bool
	PictureType   uint32
	PictureMime   string
	PictureOffset int64
	PictureLength int64
}

// FLACPictureInfo contains coordinates and metadata for an embedded picture block in a FLAC file.
type FLACPictureInfo struct {
	Type   uint32
	Mime   string
	Offset int64
	Length int64
}

// flacBlockHeader describes a FLAC metadata block header.
type flacBlockHeader struct {
	isLast     bool
	blockType  byte
	length     int
	dataOffset int64 // file offset where block data begins
}

var resizeMu sync.Mutex

// ExtractFLACTags scans a FLAC file header and metadata blocks, extracting STREAMINFO and Vorbis
// comments, as well as locating any embedded cover art without downloading the full image data.
func ExtractFLACTags(
	ctx context.Context,
	trackID string,
	reader RangeReader,
) (*FLACMetadata, *FLACPictureInfo, error) {
	if reader == nil {
		return nil, nil, errors.New("range reader is nil")
	}

	// 1. Read the initial 64 KB chunk to inspect layout
	initChunk, err := reader.ReadChunk(ctx, trackID, 0, InitialFLACScanChunk)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read initial FLAC header: %w", err)
	}
	if len(initChunk) < 8 || string(initChunk[:4]) != "fLaC" {
		return nil, nil, errors.New("not a valid FLAC stream (missing fLaC marker)")
	}

	meta := &FLACMetadata{}
	var bestPicBlock *flacBlockHeader
	var bestPicType uint32
	var bestPicOffset int64
	var bestPicLen int64
	var bestPicMime string

	// Walk metadata blocks
	var currentFileOffset int64 = 4
	bufferOffset := 4
	cachedData := initChunk

	for {
		if currentFileOffset >= MaxFLACMetadataSize {
			break
		}

		// Ensure we have at least 4 bytes for block header
		if bufferOffset+4 > len(cachedData) {
			needed := 64 * 1024
			more, err := reader.ReadChunk(ctx, trackID, currentFileOffset, needed)
			if err != nil || len(more) < 4 {
				break
			}
			cachedData = more
			bufferOffset = 0
		}

		headerByte := cachedData[bufferOffset]
		isLast := (headerByte & 0x80) != 0
		blockType := headerByte & 0x7F
		length := int(cachedData[bufferOffset+1])<<16 | int(cachedData[bufferOffset+2])<<8 | int(cachedData[bufferOffset+3])

		header := flacBlockHeader{
			isLast:     isLast,
			blockType:  blockType,
			length:     length,
			dataOffset: currentFileOffset + 4,
		}

		currentFileOffset += 4
		bufferOffset += 4

		if currentFileOffset+int64(length) > MaxFLACMetadataSize {
			break
		}

		switch blockType {
		case 0: // STREAMINFO (length 34)
			streamInfoData, err := readBlockBytes(ctx, trackID, reader, cachedData, bufferOffset, currentFileOffset, length)
			if err == nil && len(streamInfoData) >= 34 {
				parseSTREAMINFO(streamInfoData, meta)
			}

		case 4: // VORBIS_COMMENT
			commentData, err := readBlockBytes(ctx, trackID, reader, cachedData, bufferOffset, currentFileOffset, length)
			if err == nil && len(commentData) > 0 {
				parseVorbisCommentBlock(commentData, meta)
			}

		case 6: // PICTURE
			// Read the picture metadata header to determine picture type and exact image offset
			hdrLen := 1024
			if hdrLen > length {
				hdrLen = length
			}
			picHdr, err := readBlockBytes(ctx, trackID, reader, cachedData, bufferOffset, currentFileOffset, hdrLen)
			if err == nil && len(picHdr) >= 32 {
				pType, mime, imgRelOffset, imgLen, pErr := parsePictureHeader(picHdr, length)
				if pErr == nil && imgLen > 0 {
					// Prefer front cover (type 3). If already have front cover, keep it.
					// Otherwise, take type 3 or first valid picture.
					if bestPicBlock == nil || (bestPicType != 3 && pType == 3) {
						bestPicBlock = &header
						bestPicType = pType
						bestPicMime = mime
						bestPicOffset = header.dataOffset + int64(imgRelOffset)
						bestPicLen = imgLen
					}
				}
			}
		}

		currentFileOffset += int64(length)
		bufferOffset += length

		if isLast {
			break
		}
	}

	var picInfo *FLACPictureInfo
	if bestPicBlock != nil && bestPicLen > 0 {
		picInfo = &FLACPictureInfo{
			Type:   bestPicType,
			Mime:   bestPicMime,
			Offset: bestPicOffset,
			Length: bestPicLen,
		}
	}

	return meta, picInfo, nil
}

// StreamFLACPicture streams the identified embedded picture to disk, validates it, and generates a downscaled JPEG thumbnail.
func StreamFLACPicture(
	ctx context.Context,
	trackID string,
	reader RangeReader,
	picInfo *FLACPictureInfo,
	artworkDir string,
) (bool, error) {
	if picInfo == nil || picInfo.Length <= 0 || artworkDir == "" || reader == nil {
		return false, nil
	}

	if err := os.MkdirAll(artworkDir, 0755); err != nil {
		return false, fmt.Errorf("failed to create artwork directory: %w", err)
	}

	origPath := filepath.Join(artworkDir, trackID+".orig")
	thumbPath := filepath.Join(artworkDir, trackID+".jpg")

	err := streamPictureToDisk(ctx, trackID, reader, picInfo.Offset, picInfo.Length, origPath)
	if err != nil {
		return false, err
	}

	// Validate image format and decodable header
	if !isValidImage(origPath) {
		_ = os.Remove(origPath)
		return false, errors.New("downloaded picture is not a valid image format")
	}

	// Downscale to 512px JPEG thumbnail for fast list display
	if dsErr := DownscaleImageToJPEG(origPath, thumbPath, MaxThumbnailDimension); dsErr == nil {
		return true, nil
	}

	// Fallback: if downscale fails, copy orig to thumbPath if orig is valid JPEG/PNG
	if copyErr := copyFile(origPath, thumbPath); copyErr != nil {
		return false, copyErr
	}
	return true, nil
}

// ExtractFLACMetadata reads a FLAC track's layout using exact byte-range fetches,
// extracts authoritative Vorbis comments, and streams embedded cover art to disk.
// Deprecated: use ExtractEmbeddedMetadata for all container formats. Kept for test backwards-compatibility.
func ExtractFLACMetadata(
	ctx context.Context,
	trackID string,
	reader RangeReader,
	artworkDir string,
) (*FLACMetadata, bool, error) {
	meta, picInfo, err := ExtractFLACTags(ctx, trackID, reader)
	if err != nil {
		return nil, false, err
	}

	hasArt := false
	if picInfo != nil && artworkDir != "" {
		artWritten, _ := StreamFLACPicture(ctx, trackID, reader, picInfo, artworkDir)
		if artWritten {
			hasArt = true
			meta.HasPicture = true
			meta.PictureType = picInfo.Type
			meta.PictureMime = picInfo.Mime
			meta.PictureOffset = picInfo.Offset
			meta.PictureLength = picInfo.Length
		}
	}

	return meta, hasArt, nil
}

// readBlockBytes retrieves exact block bytes from cached buffer if already present, or fetches via range reader.
func readBlockBytes(
	ctx context.Context,
	trackID string,
	reader RangeReader,
	cachedData []byte,
	bufferOffset int,
	fileOffset int64,
	length int,
) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	if bufferOffset >= 0 && bufferOffset+length <= len(cachedData) {
		return cachedData[bufferOffset : bufferOffset+length], nil
	}
	return reader.ReadChunk(ctx, trackID, fileOffset, length)
}

// parseSTREAMINFO extracts sample rate, channels, total samples, and duration from FLAC STREAMINFO block (34 bytes).
func parseSTREAMINFO(data []byte, meta *FLACMetadata) {
	if len(data) < 18 {
		return
	}
	// Bytes 10..12: Sample rate (20 bits)
	sampleRate := (uint32(data[10]) << 12) | (uint32(data[11]) << 4) | (uint32(data[12]) >> 4)
	meta.SampleRate = int(sampleRate)

	// Byte 12: Channels (3 bits)
	meta.Channels = int((data[12]>>1)&0x07) + 1

	// Bytes 13..17: Total samples (36 bits)
	totalSamples := (uint64(data[13]&0x0F) << 32) |
		(uint64(data[14]) << 24) |
		(uint64(data[15]) << 16) |
		(uint64(data[16]) << 8) |
		uint64(data[17])
	meta.TotalSamples = totalSamples

	if sampleRate > 0 && totalSamples > 0 {
		meta.DurationSec = int(totalSamples / uint64(sampleRate))
	}
}

// parseVorbisCommentBlock parses all Vorbis comment key-value pairs with multi-value artist support.
func parseVorbisCommentBlock(data []byte, meta *FLACMetadata) {
	if len(data) < 8 {
		return
	}

	pos := 0
	// 1. Vendor string length (little-endian uint32)
	vendorLen := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4 + vendorLen
	if pos+4 > len(data) {
		return
	}

	// 2. User comment list count
	count := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4

	for i := 0; i < count && pos+4 <= len(data); i++ {
		length := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if pos+length > len(data) {
			break
		}

		comment := string(data[pos : pos+length])
		pos += length

		parts := strings.SplitN(comment, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		if !utf8.ValidString(val) {
			continue
		}

		switch key {
		case "TITLE":
			if meta.Title == "" {
				meta.Title = val
			}
		case "ARTIST", "PERFORMER":
			val = strings.TrimSpace(val)
			if val != "" {
				found := false
				for _, a := range meta.Artists {
					if strings.EqualFold(a, val) {
						found = true
						break
					}
				}
				if !found {
					meta.Artists = append(meta.Artists, val)
				}
			}
		case "ALBUM":
			if meta.Album == "" {
				meta.Album = val
			}
		case "ALBUMARTIST", "ALBUM ARTIST":
			if meta.AlbumArtist == "" {
				meta.AlbumArtist = val
			}
		case "DATE", "YEAR":
			if meta.Year == 0 && len(val) >= 4 {
				if y, err := strconv.Atoi(val[:4]); err == nil && y >= 1900 && y <= 2100 {
					meta.Year = y
				}
			}
		case "TRACKNUMBER", "TRACK":
			if meta.TrackNumber == 0 {
				tPart := strings.Split(val, "/")[0]
				if n, err := strconv.Atoi(strings.TrimSpace(tPart)); err == nil {
					meta.TrackNumber = n
				}
			}
		case "DISCNUMBER", "DISC":
			if meta.DiscNumber == 0 {
				dPart := strings.Split(val, "/")[0]
				if n, err := strconv.Atoi(strings.TrimSpace(dPart)); err == nil {
					meta.DiscNumber = n
				}
			}
		case "GENRE":
			if meta.Genre == "" {
				meta.Genre = val
			}
		case "COMPOSER":
			if meta.Composer == "" {
				meta.Composer = val
			}
		}
	}

	if len(meta.Artists) > 0 {
		meta.Artist = strings.Join(meta.Artists, ", ")
	}
}

// parsePictureHeader parses the header of a FLAC PICTURE metadata block to find picture type, mime,
// and the byte offset within the block where the raw image data starts.
func parsePictureHeader(data []byte, totalBlockLen int) (picType uint32, mime string, imgRelOffset int, imgLen int64, err error) {
	if len(data) < 32 {
		return 0, "", 0, 0, errors.New("picture block header too short")
	}

	pos := 0
	// 1. Picture type (uint32 big-endian)
	picType = binary.BigEndian.Uint32(data[pos : pos+4])
	pos += 4

	// 2. MIME type length
	if pos+4 > len(data) {
		return 0, "", 0, 0, errors.New("truncated mime length")
	}
	mimeLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4
	if pos+mimeLen > len(data) {
		return 0, "", 0, 0, errors.New("truncated mime string")
	}
	mime = string(data[pos : pos+mimeLen])
	pos += mimeLen

	// 3. Description length
	if pos+4 > len(data) {
		return 0, "", 0, 0, errors.New("truncated desc length")
	}
	descLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4 + descLen
	if pos+16+4 > len(data) {
		return 0, "", 0, 0, errors.New("truncated picture dimensions")
	}

	// 4. Skip width(4), height(4), color depth(4), indexed colors(4)
	pos += 16

	// 5. Picture data length
	dataLen := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4

	imgRelOffset = pos
	imgLen = dataLen
	if int64(imgRelOffset)+imgLen > int64(totalBlockLen) {
		imgLen = int64(totalBlockLen - imgRelOffset)
	}

	return picType, mime, imgRelOffset, imgLen, nil
}

// streamPictureToDisk streams raw picture bytes directly to a file on disk in bounded chunks (256 KB).
func streamPictureToDisk(
	ctx context.Context,
	trackID string,
	reader RangeReader,
	fileOffset int64,
	length int64,
	dstPath string,
) error {
	tmpPath := dstPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temp art file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}()

	const chunkSize = 256 * 1024
	remaining := length
	currOffset := fileOffset

	for remaining > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		toRead := int(remaining)
		if toRead > chunkSize {
			toRead = chunkSize
		}

		chunk, err := reader.ReadChunk(ctx, trackID, currOffset, toRead)
		if err != nil {
			return fmt.Errorf("failed reading picture chunk at offset %d: %w", currOffset, err)
		}
		if len(chunk) == 0 {
			break
		}

		if _, err := f.Write(chunk); err != nil {
			return fmt.Errorf("failed writing picture to disk: %w", err)
		}

		readBytes := int64(len(chunk))
		currOffset += readBytes
		remaining -= readBytes
	}

	_ = f.Close()
	return os.Rename(tmpPath, dstPath)
}

// IsValidImage verifies JPEG/PNG magic bytes and that image.DecodeConfig can read the header.
func IsValidImage(filePath string) bool {
	return isValidImage(filePath)
}

// isValidImage verifies JPEG/PNG magic bytes and that image.DecodeConfig can read the header.
func isValidImage(filePath string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()

	header := make([]byte, 16)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.ErrUnexpectedEOF {
		return false
	}
	if n < 8 {
		return false
	}

	isJPEG := header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF
	isPNG := bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n"))
	if !isJPEG && !isPNG {
		return false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}

	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}

	return true
}

// DownscaleImageToJPEG downscales an image on disk to fit within maxDim x maxDim, saving as JPEG.
// Processes one image at a time under resizeMu to guarantee bounded RAM usage.
func DownscaleImageToJPEG(srcPath, dstPath string, maxDim int) error {
	resizeMu.Lock()
	defer resizeMu.Unlock()

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return err
	}

	bounds := src.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w <= 0 || h <= 0 {
		return errors.New("invalid image bounds")
	}

	var dst image.Image
	if w <= maxDim && h <= maxDim {
		dst = src
	} else {
		var newW, newH int
		if w > h {
			newW = maxDim
			newH = (h * maxDim) / w
		} else {
			newH = maxDim
			newW = (w * maxDim) / h
		}
		if newW <= 0 {
			newW = 1
		}
		if newH <= 0 {
			newH = 1
		}
		dstRGBA := image.NewRGBA(image.Rect(0, 0, newW, newH))
		draw.ApproxBiLinear.Scale(dstRGBA, dstRGBA.Bounds(), src, bounds, draw.Over, nil)
		dst = dstRGBA
	}

	tmpDst := dstPath + ".tmp"
	out, err := os.Create(tmpDst)
	if err != nil {
		return err
	}

	encodeErr := jpeg.Encode(out, dst, &jpeg.Options{Quality: 85})
	_ = out.Close()
	if encodeErr != nil {
		_ = os.Remove(tmpDst)
		return encodeErr
	}

	return os.Rename(tmpDst, dstPath)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

