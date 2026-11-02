package metadata

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func buildID3v23TagWithAPIC(title, artist, album, year string, picBytes []byte) []byte {
	var body bytes.Buffer

	writeFrame := func(id, val string) {
		frameBytes := append([]byte{0}, []byte(val)...) // encoding 0 = ISO-8859-1
		body.WriteString(id)
		_ = binary.Write(&body, binary.BigEndian, uint32(len(frameBytes)))
		body.Write([]byte{0, 0}) // flags
		body.Write(frameBytes)
	}

	if title != "" {
		writeFrame("TIT2", title)
	}
	if artist != "" {
		writeFrame("TPE1", artist)
	}
	if album != "" {
		writeFrame("TALB", album)
	}
	if year != "" {
		writeFrame("TYER", year)
	}

	if len(picBytes) > 0 {
		var apic bytes.Buffer
		apic.WriteByte(0) // encoding ISO-8859-1
		apic.WriteString("image/jpeg\x00")
		apic.WriteByte(3) // cover front
		apic.WriteString("Cover\x00")
		apic.Write(picBytes)

		apicRaw := apic.Bytes()
		body.WriteString("APIC")
		_ = binary.Write(&body, binary.BigEndian, uint32(len(apicRaw)))
		body.Write([]byte{0, 0})
		body.Write(apicRaw)
	}

	raw := body.Bytes()
	tagSize := len(raw)

	var header bytes.Buffer
	header.WriteString("ID3")
	header.WriteByte(3) // version 2.3
	header.WriteByte(0) // revision
	header.WriteByte(0) // flags
	// Syncsafe tag size
	header.WriteByte(byte((tagSize >> 21) & 0x7F))
	header.WriteByte(byte((tagSize >> 14) & 0x7F))
	header.WriteByte(byte((tagSize >> 7) & 0x7F))
	header.WriteByte(byte(tagSize & 0x7F))
	header.Write(raw)

	return header.Bytes()
}

func buildFLACTagWithPicture(title, artist, album, year string, picBytes []byte) []byte {
	var vc bytes.Buffer
	vendor := "reference libFLAC"
	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(vendor)))
	vc.WriteString(vendor)

	comments := []string{
		"TITLE=" + title,
		"ARTIST=" + artist,
		"ALBUM=" + album,
		"DATE=" + year,
	}

	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(c)))
		vc.WriteString(c)
	}

	vcBytes := vc.Bytes()
	var flac bytes.Buffer
	flac.WriteString("fLaC")

	// Block header: isLast=false (0x00) | blockType=4 (Vorbis comment)
	flac.WriteByte(0x04)
	flac.WriteByte(byte((len(vcBytes) >> 16) & 0xFF))
	flac.WriteByte(byte((len(vcBytes) >> 8) & 0xFF))
	flac.WriteByte(byte(len(vcBytes) & 0xFF))
	flac.Write(vcBytes)

	// Add blockType=6 (PICTURE), isLast=true (0x86)
	if len(picBytes) > 0 {
		var picBlock bytes.Buffer
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(3)) // picture type: front cover
		mime := "image/jpeg"
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(len(mime)))
		picBlock.WriteString(mime)
		desc := "Cover"
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(len(desc)))
		picBlock.WriteString(desc)
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(500)) // width
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(500)) // height
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(24))  // depth
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(0))   // colors
		_ = binary.Write(&picBlock, binary.BigEndian, uint32(len(picBytes)))
		picBlock.Write(picBytes)

		pbData := picBlock.Bytes()
		flac.WriteByte(0x86) // isLast=true | 6
		flac.WriteByte(byte((len(pbData) >> 16) & 0xFF))
		flac.WriteByte(byte((len(pbData) >> 8) & 0xFF))
		flac.WriteByte(byte(len(pbData) & 0xFF))
		flac.Write(pbData)
	}

	return flac.Bytes()
}

func buildMP4Tag(title, artist, album, year string, picBytes []byte) []byte {
	var ilst bytes.Buffer

	writeMP4TextItem := func(fourcc, val string) {
		var dataAtom bytes.Buffer
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(16+len(val)))
		dataAtom.WriteString("data")
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(1)) // type 1 = text
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(0)) // locale
		dataAtom.WriteString(val)

		dataBytes := dataAtom.Bytes()
		_ = binary.Write(&ilst, binary.BigEndian, uint32(8+len(dataBytes)))
		ilst.WriteString(fourcc)
		ilst.Write(dataBytes)
	}

	writeMP4TextItem("\xa9nam", title)
	writeMP4TextItem("\xa9ART", artist)
	writeMP4TextItem("\xa9alb", album)
	writeMP4TextItem("\xa9day", year)

	if len(picBytes) > 0 {
		var dataAtom bytes.Buffer
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(16+len(picBytes)))
		dataAtom.WriteString("data")
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(13)) // type 13 = JPEG
		_ = binary.Write(&dataAtom, binary.BigEndian, uint32(0))
		dataAtom.Write(picBytes)

		dataBytes := dataAtom.Bytes()
		_ = binary.Write(&ilst, binary.BigEndian, uint32(8+len(dataBytes)))
		ilst.WriteString("covr")
		ilst.Write(dataBytes)
	}

	ilstBytes := ilst.Bytes()
	var ilstAtom bytes.Buffer
	_ = binary.Write(&ilstAtom, binary.BigEndian, uint32(8+len(ilstBytes)))
	ilstAtom.WriteString("ilst")
	ilstAtom.Write(ilstBytes)

	// meta atom = FullAtom (4 bytes version/flags: 00 00 00 00) + ilst
	var meta bytes.Buffer
	_ = binary.Write(&meta, binary.BigEndian, uint32(8+4+ilstAtom.Len()))
	meta.WriteString("meta")
	meta.Write([]byte{0, 0, 0, 0})
	meta.Write(ilstAtom.Bytes())
	metaBytes := meta.Bytes()

	// udta atom
	var udta bytes.Buffer
	_ = binary.Write(&udta, binary.BigEndian, uint32(8+len(metaBytes)))
	udta.WriteString("udta")
	udta.Write(metaBytes)
	udtaBytes := udta.Bytes()

	// moov atom
	var moov bytes.Buffer
	_ = binary.Write(&moov, binary.BigEndian, uint32(8+len(udtaBytes)))
	moov.WriteString("moov")
	moov.Write(udtaBytes)
	moovBytes := moov.Bytes()

	// ftyp atom
	var ftypAtom bytes.Buffer
	ftypPayload := []byte("M4A \x00\x00\x00\x00")
	_ = binary.Write(&ftypAtom, binary.BigEndian, uint32(8+len(ftypPayload)))
	ftypAtom.WriteString("ftyp")
	ftypAtom.Write(ftypPayload)

	var full bytes.Buffer
	full.Write(ftypAtom.Bytes())
	full.Write(moovBytes)
	return full.Bytes()
}

func TestParseID3v2WithPicture(t *testing.T) {
	fakePic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, 0x01, 0x01, 0x00, 0x48, 0x00, 0x48, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43, 0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09}
	data := buildID3v23TagWithAPIC("Gul", "Anuv Jain", "Gul - Single", "2021", fakePic)

	meta := ParseAudioHeader(data)
	if meta == nil {
		t.Fatalf("ParseAudioHeader returned nil")
	}

	if meta.Title != "Gul" {
		t.Errorf("Expected title 'Gul', got %q", meta.Title)
	}
	if meta.Artist != "Anuv Jain" {
		t.Errorf("Expected artist 'Anuv Jain', got %q", meta.Artist)
	}
	if meta.Album != "Gul - Single" {
		t.Errorf("Expected album 'Gul - Single', got %q", meta.Album)
	}
	if meta.Year != 2021 {
		t.Errorf("Expected year 2021, got %d", meta.Year)
	}
	if len(meta.Picture) == 0 {
		t.Fatalf("Expected picture bytes, got empty")
	}
	if !bytes.Equal(meta.Picture, fakePic) {
		t.Errorf("Picture bytes mismatch")
	}
	if meta.PictureMime != "image/jpeg" {
		t.Errorf("Expected image/jpeg, got %s", meta.PictureMime)
	}
}

func TestParseFLACWithPicture(t *testing.T) {
	fakePic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, 0x01, 0x01, 0x00, 0x48, 0x00, 0x48, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43, 0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09}
	data := buildFLACTagWithPicture("Husn", "Anuv Jain", "Husn - Single", "2023", fakePic)

	meta := ParseAudioHeader(data)
	if meta == nil {
		t.Fatalf("ParseAudioHeader returned nil for FLAC tag")
	}

	if meta.Title != "Husn" {
		t.Errorf("Expected title 'Husn', got %q", meta.Title)
	}
	if meta.Artist != "Anuv Jain" {
		t.Errorf("Expected artist 'Anuv Jain', got %q", meta.Artist)
	}
	if meta.Album != "Husn - Single" {
		t.Errorf("Expected album 'Husn - Single', got %q", meta.Album)
	}
	if meta.Year != 2023 {
		t.Errorf("Expected year 2023, got %d", meta.Year)
	}
	if len(meta.Picture) == 0 {
		t.Fatalf("Expected picture bytes, got empty")
	}
	if !bytes.Equal(meta.Picture, fakePic) {
		t.Errorf("Picture bytes mismatch")
	}
}

func TestParseMP4WithCover(t *testing.T) {
	fakePic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, 0x01, 0x01, 0x00, 0x48, 0x00, 0x48, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43, 0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09}
	data := buildMP4Tag("Guzarish", "A.R. Rahman", "Ghajini", "2008", fakePic)

	meta := ParseAudioHeader(data)
	if meta == nil {
		t.Fatalf("ParseAudioHeader returned nil for MP4")
	}

	if meta.Title != "Guzarish" {
		t.Errorf("Expected title 'Guzarish', got %q", meta.Title)
	}
	if meta.Artist != "A.R. Rahman" {
		t.Errorf("Expected artist 'A.R. Rahman', got %q", meta.Artist)
	}
	if meta.Album != "Ghajini" {
		t.Errorf("Expected album 'Ghajini', got %q", meta.Album)
	}
	if meta.Year != 2008 {
		t.Errorf("Expected year 2008, got %d", meta.Year)
	}
	if len(meta.Picture) == 0 {
		t.Fatalf("Expected picture bytes, got empty")
	}
	if !bytes.Equal(meta.Picture, fakePic) {
		t.Errorf("Picture bytes mismatch")
	}
}

func TestParseCorruptedOrEmptyHeader(t *testing.T) {
	if meta := ParseAudioHeader(nil); meta != nil {
		t.Errorf("Expected nil for nil slice, got %+v", meta)
	}

	if meta := ParseAudioHeader([]byte{0, 1, 2, 3}); meta != nil {
		t.Errorf("Expected nil for invalid bytes, got %+v", meta)
	}
}
