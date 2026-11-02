package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"musix/engine/internal/models"
)

// Client defines the interface for MTProto streaming and track resolution.
type Client interface {
	Start(ctx context.Context) error
	Stop()
	IsReady() bool
	DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error)
	ResolveTrack(ctx context.Context, trackID string) (*models.Track, error)
	RegisterTrack(track *models.Track)
}

// GotdClient wraps a single or primary gotd MTProto client connection.
type GotdClient struct {
	config     *models.Config
	client     *telegram.Client
	api        *tg.Client
	ready      bool
	mu         sync.RWMutex
	cancel     context.CancelFunc
	tracks     map[string]*models.Track
	tracksMu   sync.RWMutex
}

// NewGotdClient creates an unstarted gotd Telegram client.
func NewGotdClient(cfg *models.Config) (*GotdClient, error) {
	if cfg.ApiID <= 0 || cfg.ApiHash == "" {
		return nil, errors.New("api_id and api_hash are required")
	}

	sessionDir := filepath.Join(cfg.FilesDir, "sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		sessionDir = filepath.Join(os.TempDir(), "gotd_sessions")
		_ = os.MkdirAll(sessionDir, 0700)
	}

	tokenPrefix := "session"
	if cfg.BotToken != "" {
		parts := strings.Split(cfg.BotToken, ":")
		if len(parts) > 0 {
			tokenPrefix = parts[0]
		}
	}
	sessionPath := filepath.Join(sessionDir, fmt.Sprintf("session_%s.json", tokenPrefix))

	opts := telegram.Options{
		NoUpdates:      true,
		SessionStorage: &session.FileStorage{Path: sessionPath},
		DialTimeout:    15 * time.Second,
		Resolver: dcs.Plain(dcs.PlainOptions{
			Protocol:   transport.Abridged,
			Obfuscated: true,
			Network:    "tcp4",
		}),
	}

	tgClient := telegram.NewClient(cfg.ApiID, cfg.ApiHash, opts)

	return &GotdClient{
		config:   cfg,
		client:   tgClient,
		api:      tgClient.API(),
		tracks:   make(map[string]*models.Track),
	}, nil
}

func (c *GotdClient) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	readyChan := make(chan struct{})
	var once sync.Once

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := c.client.Run(ctx, func(runCtx context.Context) error {
				if c.config.BotToken != "" {
					status, err := c.client.Auth().Status(runCtx)
					if err == nil && !status.Authorized {
						if _, err := c.client.Auth().Bot(runCtx, c.config.BotToken); err != nil {
							return err
						}
					}
				}

				c.mu.Lock()
				c.ready = true
				c.mu.Unlock()

				once.Do(func() {
					close(readyChan)
				})

				<-runCtx.Done()
				return runCtx.Err()
			})

			c.mu.Lock()
			c.ready = false
			c.mu.Unlock()

			if ctx.Err() != nil {
				return
			}
			if err != nil {
				time.Sleep(2 * time.Second)
			}
		}
	}()

	select {
	case <-readyChan:
		return nil
	case <-time.After(10 * time.Second):
		// Allow startup to proceed even if connection is still establishing
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *GotdClient) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
}

func (c *GotdClient) IsReady() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

func (c *GotdClient) RegisterTrack(track *models.Track) {
	c.tracksMu.Lock()
	defer c.tracksMu.Unlock()
	c.tracks[track.ID] = track
}

func (c *GotdClient) ResolveTrack(ctx context.Context, trackID string) (*models.Track, error) {
	c.tracksMu.RLock()
	defer c.tracksMu.RUnlock()
	t, ok := c.tracks[trackID]
	if !ok {
		return nil, fmt.Errorf("track not found: %s", trackID)
	}
	return t, nil
}

func (c *GotdClient) DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	if !c.IsReady() {
		return nil, errors.New("telegram client not ready")
	}

	req := &tg.UploadGetFileRequest{
		Precise:  true,
		Location: location,
		Offset:   offset,
		Limit:    limit,
	}

	res, err := c.api.UploadGetFile(ctx, req)
	if err != nil {
		return nil, err
	}

	switch f := res.(type) {
	case *tg.UploadFile:
		return f.Bytes, nil
	default:
		return nil, fmt.Errorf("unexpected UploadGetFile type: %T", res)
	}
}

// MockClient is an offline client for testing and local spikes.
type MockClient struct {
	mu      sync.RWMutex
	ready   bool
	tracks  map[string]*models.Track
	payload map[string][]byte
}

func NewMockClient() *MockClient {
	return &MockClient{
		ready:   true,
		tracks:  make(map[string]*models.Track),
		payload: make(map[string][]byte),
	}
}

func (m *MockClient) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	return nil
}

func (m *MockClient) Stop() {
	m.mu.Lock()
	m.ready = false
	m.mu.Unlock()
}

func (m *MockClient) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *MockClient) RegisterTrackWithPayload(track *models.Track, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	track.FileSize = int64(len(data))
	m.tracks[track.ID] = track
	m.payload[track.ID] = data
}

func (m *MockClient) RegisterTrack(track *models.Track) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracks[track.ID] = track
}

func (m *MockClient) ResolveTrack(ctx context.Context, trackID string) (*models.Track, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tracks[trackID]
	if !ok {
		return nil, fmt.Errorf("track not found: %s", trackID)
	}
	return t, nil
}

func (m *MockClient) DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Find payload matching location ID
	var data []byte
	for _, t := range m.tracks {
		if t.AccessHash == location.AccessHash || (location.ID != 0 && t.ID == fmt.Sprintf("%d", location.ID)) {
			data = m.payload[t.ID]
			break
		}
	}
	if data == nil {
		// Return synthetic bytes if no specific payload registered
		if offset >= 10*1024*1024 {
			return nil, nil // EOF
		}
		end := offset + int64(limit)
		if end > 10*1024*1024 {
			end = 10 * 1024 * 1024
		}
		chunk := make([]byte, end-offset)
		for i := range chunk {
			chunk[i] = byte((offset + int64(i)) % 256)
		}
		return chunk, nil
	}

	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + int64(limit)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

