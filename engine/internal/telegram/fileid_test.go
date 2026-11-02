package telegram

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeFileID(t *testing.T) {
	mediaID := int64(1234567890123456)
	accessHash := int64(9876543210987654)
	dcID := int32(2)
	fileRef := []byte{0x01, 0x02, 0x03, 0x04, 0xaa, 0xbb, 0xcc}

	encoded := EncodeFileID(mediaID, accessHash, dcID, fileRef)
	if encoded == "" {
		t.Fatal("EncodeFileID returned empty string")
	}

	decoded, err := DecodeFileID(encoded)
	if err != nil {
		t.Fatalf("DecodeFileID failed: %v", err)
	}

	if decoded.MediaID != mediaID {
		t.Errorf("MediaID: got %d, want %d", decoded.MediaID, mediaID)
	}
	if decoded.AccessHash != accessHash {
		t.Errorf("AccessHash: got %d, want %d", decoded.AccessHash, accessHash)
	}
	if decoded.DCID != dcID {
		t.Errorf("DCID: got %d, want %d", decoded.DCID, dcID)
	}
	if !bytes.Equal(decoded.FileReference, fileRef) {
		t.Errorf("FileReference: got %v, want %v", decoded.FileReference, fileRef)
	}
}

