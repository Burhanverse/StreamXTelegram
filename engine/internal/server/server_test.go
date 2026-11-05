package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musix/engine/internal/models"
	"musix/engine/internal/telegram"
)

func setupTestServer(t *testing.T) (*Server, *telegram.MockClient, []byte) {
	mockClient := telegram.NewMockClient()
	_ = mockClient.Start(context.Background())

	// Create a 1 MB test audio buffer
	testData := make([]byte, 1024*1024)
	for i := range testData {
		testData[i] = byte(i % 251)
	}

	track := &models.Track{
		ID:          "track_123",
		Title:       "Test Track",
		Artist:      "Test Artist",
		DurationSec: 180,
		MimeType:    "audio/mpeg",
		FileSize:    int64(len(testData)),
		FileName:    "test.mp3",
		AccessHash:  12345,
	}
	mockClient.RegisterTrackWithPayload(track, testData)

	srv, err := New(Config{
		Token:  "test-secret-token",
		Port:   0,
		Client: mockClient,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	srv.Start()

	t.Cleanup(func() {
		_ = srv.Close()
		mockClient.Stop()
	})

	return srv, mockClient, testData
}

func TestServerAuth(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d/stream/track_123", srv.Port())

	// 1. Missing token -> 401
	resp, err := http.Get(baseURL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 for missing token, got %d", resp.StatusCode)
	}

	// 2. Wrong token -> 401
	resp, err = http.Get(baseURL + "?token=wrong")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 for wrong token, got %d", resp.StatusCode)
	}

	// 3. Valid token query param -> 200
	resp, err = http.Get(baseURL + "?token=" + srv.Token())
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 for valid token, got %d", resp.StatusCode)
	}

	// 4. Valid token in Authorization Bearer header -> 200
	req, _ := http.NewRequest(http.MethodGet, baseURL, nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 for Bearer token, got %d", resp.StatusCode)
	}
}

func TestStreamRangeRequests(t *testing.T) {
	srv, _, testData := setupTestServer(t)
	streamURL := fmt.Sprintf("http://127.0.0.1:%d/stream/track_123?token=%s", srv.Port(), srv.Token())

	// Test 1: Full stream
	resp, err := http.Get(streamURL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if int64(len(body)) != int64(len(testData)) {
		t.Errorf("body length = %d, want %d", len(body), len(testData))
	}
	if !bytes.Equal(body, testData) {
		t.Errorf("body content mismatch")
	}

	// Test 2: Range request (seek) bytes=1024-2047
	req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
	req.Header.Set("Range", "bytes=1024-2047")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range request failed: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 1024-2047/%d", len(testData)) {
		t.Errorf("Content-Range = %s, want bytes 1024-2047/%d", cr, len(testData))
	}
	if len(body) != 1024 {
		t.Errorf("body len = %d, want 1024", len(body))
	}
	if !bytes.Equal(body, testData[1024:2048]) {
		t.Errorf("seeked range content mismatch")
	}

	// Test 3: Suffix Range request bytes=-1000
	req, _ = http.NewRequest(http.MethodGet, streamURL, nil)
	req.Header.Set("Range", "bytes=-1000")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Suffix range request failed: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	expectedStart := int64(len(testData) - 1000)
	if !bytes.Equal(body, testData[expectedStart:]) {
		t.Errorf("suffix range content mismatch")
	}

	// Test 4: Unsatisfiable Range request bytes=2000000-
	req, _ = http.NewRequest(http.MethodGet, streamURL, nil)
	req.Header.Set("Range", "bytes=2000000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Unsatisfiable range request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}
}

func TestLatencyMeasurements(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	streamURL := fmt.Sprintf("http://127.0.0.1:%d/stream/track_123?token=%s", srv.Port(), srv.Token())

	// Measure First-Byte Latency (TTFB)
	start := time.Now()
	resp, err := http.Get(streamURL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	var firstByte [1]byte
	_, err = io.ReadFull(resp.Body, firstByte[:])
	ttfb := time.Since(start)
	resp.Body.Close()

	if err != nil {
		t.Fatalf("failed to read first byte: %v", err)
	}
	t.Logf("Measured First-Byte Latency (TTFB): %v", ttfb)

	// Measure Seek Latency (Range request TTFB to arbitrary offset)
	seekStart := time.Now()
	req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
	req.Header.Set("Range", "bytes=524288-1048575") // Seek to 512KB mark
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Seek request failed: %v", err)
	}
	_, err = io.ReadFull(resp.Body, firstByte[:])
	seekLatency := time.Since(seekStart)
	resp.Body.Close()

	if err != nil {
		t.Fatalf("failed to read first byte after seek: %v", err)
	}
	t.Logf("Measured Seek Latency: %v", seekLatency)

	if ttfb > 100*time.Millisecond {
		t.Errorf("first byte latency too high for local loopback: %v", ttfb)
	}
	if seekLatency > 100*time.Millisecond {
		t.Errorf("seek latency too high for local loopback: %v", seekLatency)
	}
}

func TestHTTPSArtworkServing(t *testing.T) {
	artDir := t.TempDir()
	testArtBytes := []byte("jpeg-image-content-for-testing")
	if err := os.WriteFile(filepath.Join(artDir, "track_https_1.jpg"), testArtBytes, 0644); err != nil {
		t.Fatalf("failed to write test artwork: %v", err)
	}

	srv, err := New(Config{
		Token:      "test-token",
		Port:       0,
		ArtworkDir: artDir,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	srv.Start()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	if srv.HTTPSPort() == 0 {
		t.Fatal("expected HTTPSPort > 0")
	}

	tlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName: "local.burhanverse.eu.org",
			},
		},
	}

	url := fmt.Sprintf("https://127.0.0.1:%d/artwork/track_https_1.jpg?token=%s", srv.HTTPSPort(), srv.Token())
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Host = "local.burhanverse.eu.org"

	resp, err := tlsClient.Do(req)
	if err != nil {
		t.Fatalf("HTTPS GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("expected Content-Type image/jpeg, got %s", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if !bytes.Equal(body, testArtBytes) {
		t.Errorf("artwork body mismatch: got %d bytes, want %d bytes", len(body), len(testArtBytes))
	}
}
