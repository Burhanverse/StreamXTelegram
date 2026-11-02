package metadata

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"
	"unicode/utf16"
)

// AudioHeaderMeta holds metadata and cover art extracted directly from audio file header bytes.
type AudioHeaderMeta struct {
	Title       string
	Artist      string
	Album       string
	Year        int
	Picture     []byte
	PictureMime string
}

// ParseAudioHeader inspects the first chunk of audio bytes (typically 32 KB - 1 MB)
// and extracts ID3v2 (MP3), FLAC, Ogg Vorbis, or MP4/M4A metadata & embedded cover art.
func ParseAudioHeader(data []byte) *AudioHeaderMeta {
	if len(data) < 8 {
		return nil
	}

	// 1. Check for ID3v2 header ("ID3")
	if bytes.HasPrefix(data, []byte("ID3")) {
		if meta := parseID3v2(data); meta != nil {
			return meta
		}
	}

	// 2. Check for FLAC header ("fLaC")
	if bytes.HasPrefix(data, []byte("fLaC")) {
		if meta := parseFLAC(data); meta != nil {
			return meta
		}
	}

	// 3. Check for Ogg Vorbis header ("OggS")
	if bytes.HasPrefix(data, []byte("OggS")) {
		if meta := parseOggVorbis(data); meta != nil {
			return meta
		}
	}

	// 4. Check for MP4 / M4A (starts with ftyp atom or contains ftyp/moov)
	if len(data) >= 12 && (string(data[4:8]) == "ftyp" || string(data[4:8]) == "moov") {
		if meta := parseMP4(data); meta != nil {
			return meta
		}
	}

	return nil
}

func parseID3v2(data []byte) *AudioHeaderMeta {
	if len(data) < 10 || string(data[:3]) != "ID3" {
		return nil
	}

	version := data[3]
	if version < 2 || version > 4 {
		return nil
	}

	// Calculate tag size from 4 syncsafe integer bytes
	tagSize := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
	if tagSize > len(data)-10 {
		tagSize = len(data) - 10
	}

	pos := 10
	end := pos + tagSize
	meta := &AudioHeaderMeta{}

	for pos+10 <= end {
		frameID := string(data[pos : pos+4])
		// Check for null padding at end of ID3 tag
		if data[pos] == 0 {
			break
		}

		var frameSize int
		if version == 4 {
			// ID3v2.4 uses syncsafe integer for frame size
			frameSize = int(data[pos+4]&0x7F)<<21 | int(data[pos+5]&0x7F)<<14 | int(data[pos+6]&0x7F)<<7 | int(data[pos+7]&0x7F)
		} else {
			// ID3v2.3 uses big-endian uint32
			frameSize = int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		}

		pos += 10
		if frameSize <= 0 || pos+frameSize > len(data) {
			break
		}

		frameData := data[pos : pos+frameSize]
		pos += frameSize

		// Check for embedded picture frame (APIC in ID3v2.3/2.4, PIC in ID3v2.2)
		if frameID == "APIC" || frameID == "PIC" {
			if pic, mime := extractAPICPicture(frameData); len(pic) > 0 {
				meta.Picture = pic
				meta.PictureMime = mime
			}
			continue
		}

		val := decodeID3Text(frameData)
		if val == "" {
			continue
		}

		switch frameID {
		case "TIT2": // Title
			meta.Title = val
		case "TPE1", "TPE2": // Artist / Album Artist
			if meta.Artist == "" {
				meta.Artist = val
			}
		case "TALB": // Album
			meta.Album = val
		case "TYER", "TDRC": // Year / Recording Time
			if len(val) >= 4 {
				if y, err := strconv.Atoi(val[:4]); err == nil && y >= 1900 && y <= 2100 {
					meta.Year = y
				}
			}
		}
	}

	meta.Title, meta.Artist = CleanMetadata(meta.Title, meta.Artist)
	return meta
}

func extractAPICPicture(data []byte) ([]byte, string) {
	if len(data) < 4 {
		return nil, ""
	}

	// Fast detection: search for known image file signatures within the frame
	// JPEG magic: 0xFF, 0xD8, 0xFF
	if idx := bytes.Index(data, []byte{0xFF, 0xD8, 0xFF}); idx != -1 {
		return data[idx:], "image/jpeg"
	}

	// PNG magic: 0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'
	if idx := bytes.Index(data, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}); idx != -1 {
		return data[idx:], "image/png"
	}

	// WebP magic: 'R', 'I', 'F', 'F', ... 'W', 'E', 'B', 'P'
	if idx := bytes.Index(data, []byte("WEBP")); idx >= 8 && bytes.HasPrefix(data[idx-8:], []byte("RIFF")) {
		return data[idx-8:], "image/webp"
	}

	// Specification fallback:
	// Byte 0: encoding
	// Next: null-terminated MIME type string
	// Next: 1 byte picture type
	// Next: description (terminated by null or double null)
	// Next: picture data
	pos := 1
	for pos < len(data) && data[pos] != 0 {
		pos++
	}
	mime := "image/jpeg"
	if pos < len(data) {
		rawMime := string(data[1:pos])
		if rawMime != "" {
			mime = rawMime
		}
		pos++ // skip null byte
	}
	if pos < len(data) {
		pos++ // skip picture type
	}

	encoding := data[0]
	if encoding == 1 || encoding == 2 {
		for pos+1 < len(data) {
			if data[pos] == 0 && data[pos+1] == 0 {
				pos += 2
				break
			}
			pos += 2
		}
	} else {
		for pos < len(data) && data[pos] != 0 {
			pos++
		}
		if pos < len(data) {
			pos++
		}
	}

	if pos < len(data) {
		pic := data[pos:]
		if len(pic) > 32 {
			return pic, mime
		}
	}

	return nil, ""
}

func decodeID3Text(data []byte) string {
	if len(data) <= 1 {
		return ""
	}

	encoding := data[0]
	raw := data[1:]

	switch encoding {
	case 0: // ISO-8859-1 (Latin1)
		return strings.Trim(strings.Trim(string(raw), "\x00"), " ")
	case 1: // UTF-16 with BOM
		if len(raw) < 2 {
			return ""
		}
		var bom binary.ByteOrder = binary.BigEndian
		start := 2
		if raw[0] == 0xFF && raw[1] == 0xFE {
			bom = binary.LittleEndian
		} else if raw[0] == 0xFE && raw[1] == 0xFF {
			bom = binary.BigEndian
		} else {
			start = 0
		}

		u16 := make([]uint16, 0, len(raw)/2)
		for i := start; i+1 < len(raw); i += 2 {
			val := bom.Uint16(raw[i : i+2])
			if val == 0 {
				break
			}
			u16 = append(u16, val)
		}
		return strings.TrimSpace(string(utf16.Decode(u16)))
	case 2: // UTF-16BE without BOM
		u16 := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			val := binary.BigEndian.Uint16(raw[i : i+2])
			if val == 0 {
				break
			}
			u16 = append(u16, val)
		}
		return strings.TrimSpace(string(utf16.Decode(u16)))
	case 3: // UTF-8
		return strings.Trim(strings.Trim(string(raw), "\x00"), " ")
	default:
		return strings.Trim(strings.Trim(string(raw), "\x00"), " ")
	}
}

func parseFLAC(data []byte) *AudioHeaderMeta {
	if len(data) < 8 || string(data[:4]) != "fLaC" {
		return nil
	}

	pos := 4
	meta := &AudioHeaderMeta{}

	for pos+4 <= len(data) {
		header := data[pos]
		isLast := (header & 0x80) != 0
		blockType := header & 0x7F

		length := int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4

		if pos+length > len(data) {
			break
		}

		blockData := data[pos : pos+length]

		// Block type 4 is VORBIS_COMMENT
		if blockType == 4 {
			if vcMeta := parseVorbisComment(blockData); vcMeta != nil {
				if meta.Title == "" {
					meta.Title = vcMeta.Title
				}
				if meta.Artist == "" {
					meta.Artist = vcMeta.Artist
				}
				if meta.Album == "" {
					meta.Album = vcMeta.Album
				}
				if meta.Year == 0 {
					meta.Year = vcMeta.Year
				}
				if len(meta.Picture) == 0 && len(vcMeta.Picture) > 0 {
					meta.Picture = vcMeta.Picture
					meta.PictureMime = vcMeta.PictureMime
				}
			}
		}

		// Block type 6 is PICTURE
		if blockType == 6 {
			if pic, mime := parseFLACPicture(blockData); len(pic) > 0 {
				meta.Picture = pic
				meta.PictureMime = mime
			}
		}

		if isLast {
			break
		}
		pos += length
	}

	meta.Title, meta.Artist = CleanMetadata(meta.Title, meta.Artist)
	return meta
}

func parseFLACPicture(data []byte) ([]byte, string) {
	if len(data) < 32 {
		return nil, ""
	}

	pos := 4 // skip picture type uint32
	mimeLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4
	if pos+mimeLen > len(data) {
		return nil, ""
	}
	mime := string(data[pos : pos+mimeLen])
	pos += mimeLen

	if pos+4 > len(data) {
		return nil, ""
	}
	descLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4 + descLen

	pos += 16 // skip width(4), height(4), color depth(4), indexed colors(4)
	if pos+4 > len(data) {
		return nil, ""
	}
	picLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
	pos += 4

	if picLen <= 0 || pos+picLen > len(data) {
		if pos < len(data) && len(data)-pos > 32 {
			return data[pos:], mime
		}
		return nil, ""
	}

	return data[pos : pos+picLen], mime
}

func parseOggVorbis(data []byte) *AudioHeaderMeta {
	marker := []byte("\x03vorbis")
	idx := bytes.Index(data, marker)
	if idx == -1 || idx+len(marker) >= len(data) {
		return nil
	}

	return parseVorbisComment(data[idx+len(marker):])
}

func parseVorbisComment(data []byte) *AudioHeaderMeta {
	if len(data) < 8 {
		return nil
	}

	pos := 0

	// Vendor string length (little-endian uint32)
	vendorLen := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4 + vendorLen
	if pos+4 > len(data) {
		return nil
	}

	// Comment count (little-endian uint32)
	count := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4

	meta := &AudioHeaderMeta{}

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

		switch key {
		case "TITLE":
			meta.Title = val
		case "ARTIST", "PERFORMER", "ALBUMARTIST":
			if meta.Artist == "" {
				meta.Artist = val
			}
		case "ALBUM":
			meta.Album = val
		case "DATE", "YEAR":
			if len(val) >= 4 {
				if y, err := strconv.Atoi(val[:4]); err == nil && y >= 1900 && y <= 2100 {
					meta.Year = y
				}
			}
		case "METADATA_BLOCK_PICTURE":
			if decoded, err := base64.StdEncoding.DecodeString(val); err == nil {
				if pic, mime := parseFLACPicture(decoded); len(pic) > 0 {
					meta.Picture = pic
					meta.PictureMime = mime
				}
			}
		}
	}

	meta.Title, meta.Artist = CleanMetadata(meta.Title, meta.Artist)
	return meta
}

// parseMP4 walks atoms in MP4/M4A containers searching for ilst metadata & covr artwork.
func parseMP4(data []byte) *AudioHeaderMeta {
	meta := &AudioHeaderMeta{}
	walkMP4Atoms(data, meta)
	meta.Title, meta.Artist = CleanMetadata(meta.Title, meta.Artist)
	if meta.Title != "" || meta.Artist != "" || meta.Album != "" || len(meta.Picture) > 0 {
		return meta
	}
	return nil
}

func walkMP4Atoms(data []byte, meta *AudioHeaderMeta) {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		atomType := string(data[pos+4 : pos+8])

		atomHeaderSize := 8
		if size == 1 && pos+16 <= len(data) { // 64-bit extended size
			size = int(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			atomHeaderSize = 16
		} else if size == 0 { // extends to end of file
			size = len(data) - pos
		}

		if size < atomHeaderSize || pos+size > len(data) {
			break
		}

		payload := data[pos+atomHeaderSize : pos+size]

		switch atomType {
		case "moov", "udta":
			walkMP4Atoms(payload, meta)
		case "meta":
			// 'meta' atom in MP4 is a FullAtom: contains 4 bytes flags/version before children
			if len(payload) > 4 {
				walkMP4Atoms(payload[4:], meta)
			}
		case "ilst":
			parseMP4Ilst(payload, meta)
		}

		pos += size
	}
}

func parseMP4Ilst(data []byte, meta *AudioHeaderMeta) {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		tagType := string(data[pos+4 : pos+8])

		if size < 8 || pos+size > len(data) {
			break
		}

		tagData := data[pos+8 : pos+size]
		pos += size

		// Inside tagData is typically a 'data' atom:
		// 4 bytes size, 4 bytes "data", 4 bytes type, 4 bytes flags/locale, payload
		text, pic, mime := parseMP4DataAtom(tagData)

		switch tagType {
		case "\xa9nam": // Title
			meta.Title = text
		case "\xa9ART", "aART": // Artist / Album Artist
			if meta.Artist == "" {
				meta.Artist = text
			}
		case "\xa9alb": // Album
			meta.Album = text
		case "\xa9day": // Year
			if len(text) >= 4 {
				if y, err := strconv.Atoi(text[:4]); err == nil && y >= 1900 && y <= 2100 {
					meta.Year = y
				}
			}
		case "covr": // Cover Art
			if len(pic) > 0 {
				meta.Picture = pic
				meta.PictureMime = mime
			}
		}
	}
}

func parseMP4DataAtom(data []byte) (string, []byte, string) {
	pos := 0
	for pos+16 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		boxType := string(data[pos+4 : pos+8])

		if size < 16 || pos+size > len(data) {
			break
		}

		if boxType == "data" {
			dataType := binary.BigEndian.Uint32(data[pos+8 : pos+12])
			payload := data[pos+16 : pos+size]

			// Data types: 1 = UTF-8 text, 13 = JPEG, 14 = PNG, 0 = implicit
			if dataType == 1 {
				return strings.TrimSpace(string(payload)), nil, ""
			} else if dataType == 13 || bytes.HasPrefix(payload, []byte{0xFF, 0xD8, 0xFF}) {
				return "", payload, "image/jpeg"
			} else if dataType == 14 || bytes.HasPrefix(payload, []byte{0x89, 0x50, 0x4E, 0x47}) {
				return "", payload, "image/png"
			} else if len(payload) > 32 {
				// Fallback binary detection
				if bytes.Index(payload, []byte{0xFF, 0xD8, 0xFF}) != -1 {
					return "", payload, "image/jpeg"
				}
				return "", payload, "image/jpeg"
			}
		}
		pos += size
	}
	return "", nil, ""
}
