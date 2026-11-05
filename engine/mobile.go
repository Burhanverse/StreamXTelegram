package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/logger"
	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
	"musix/engine/internal/server"
	"musix/engine/internal/services"
	"musix/engine/internal/telegram"
)

var (
	globalServer      *server.Server
	globalClient      telegram.Client
	globalDB          *database.DB
	globalEnrichment  *services.EnrichmentService
	globalCatchup     *telegram.CatchupService
	globalBackup      *telegram.BackupService
	globalWatcher     *telegram.Watcher
	globalCallback    EventCallback
	globalCallbackMu  sync.RWMutex
	globalMu          sync.Mutex
	log               = logger.New("mobile")
)

func init() {
	logger.SetListener(func(line string) {
		notifyLog(line)
	})
}

// EventCallback allows Android/Kotlin to receive indexing progress and error events.
type EventCallback interface {
	OnProgress(status string, current, total int)
	OnError(message string)
	OnLog(line string)
}

// SetEventCallback registers an event callback for mobile progress and error notifications.
func SetEventCallback(cb EventCallback) {
	globalCallbackMu.Lock()
	defer globalCallbackMu.Unlock()
	globalCallback = cb
}

func notifyProgress(status string, current, total int) {
	globalCallbackMu.RLock()
	cb := globalCallback
	globalCallbackMu.RUnlock()
	if cb != nil {
		cb.OnProgress(status, current, total)
	}
}

func notifyError(msg string) {
	globalCallbackMu.RLock()
	cb := globalCallback
	globalCallbackMu.RUnlock()
	if cb != nil {
		cb.OnError(msg)
	}
}

func notifyLog(line string) {
	globalCallbackMu.RLock()
	cb := globalCallback
	globalCallbackMu.RUnlock()
	if cb != nil {
		cb.OnLog(line)
	}
}


// StartResult returns the loopback ports and session security token.
type StartResult struct {
	Port      int
	HTTPSPort int
	Token     string
}

// Start launches the engine, embedded database, MTProto pool, and loopback server.
func Start(filesDir, cacheDir, configJSON string) (*StartResult, error) {
	globalMu.Lock()
	defer globalMu.Unlock()

	if globalServer != nil {
		return &StartResult{
			Port:      globalServer.Port(),
			HTTPSPort: globalServer.HTTPSPort(),
			Token:     globalServer.Token(),
		}, nil
	}

	var cfg models.Config
	if configJSON != "" {
		_ = json.Unmarshal([]byte(configJSON), &cfg)
	}
	cfg.FilesDir = filesDir
	cfg.CacheDir = cacheDir

	// Fallback to bot_tokens if primary bot_token is empty
	if cfg.BotToken == "" && len(cfg.BotTokens) > 0 {
		cfg.BotToken = cfg.BotTokens[0]
		if len(cfg.BotTokens) > 1 && len(cfg.WorkerTokens) == 0 {
			cfg.WorkerTokens = cfg.BotTokens[1:]
		}
	}

	// Initialize embedded SQLite database
	var db *database.DB
	if filesDir != "" {
		dbPath := filepath.Join(filesDir, "library.db")
		if d, err := database.Open(dbPath); err == nil {
			db = d
			globalDB = d
		} else {
			log.Errorf("Failed to open SQLite database at %s: %v", dbPath, err)
		}
	}

	var client telegram.Client
	if cfg.ApiID > 0 && cfg.BotToken != "" {
		log.Infof("Configuring Telegram MTProto pool with API ID %d and %d bots for chat %d...", cfg.ApiID, 1+len(cfg.WorkerTokens), cfg.ChatID)
		pool, err := telegram.NewPool(&cfg, db)
		if err != nil {
			log.Errorf("Failed to initialize Telegram pool: %v", err)
			notifyError(fmt.Sprintf("Failed to initialize Telegram pool: %v", err))
		} else {
			client = pool
			if db != nil {
				globalEnrichment = services.NewEnrichmentService(db)
			}
			globalCatchup = telegram.NewCatchupService(pool, db, globalEnrichment)
			globalBackup = telegram.NewBackupService(pool, db)
			if err := client.Start(context.Background()); err != nil {
				log.Errorf("Failed to start Telegram client: %v", err)
				notifyError(fmt.Sprintf("Failed to start Telegram client: %v", err))
			} else {
				log.Infof("Telegram client started successfully")
				if globalCatchup != nil && cfg.ChatID != 0 {
					globalWatcher = globalCatchup.StartWatcher(context.Background(), cfg.ChatID, 30*time.Second)
				}
			}
		}
	} else {
		log.Infof("Telegram credentials not configured; running in mock/offline mode")
	}


	if client == nil {
		mockClient := telegram.NewMockClient()
		client = mockClient
		_ = client.Start(context.Background())
	}
	globalClient = client

	var artworkDir string
	if filesDir != "" {
		artworkDir = filepath.Join(filesDir, metadata.ArtworkDirName)
		_ = os.MkdirAll(artworkDir, 0755)
	}

	if client != nil && db != nil && globalEnrichment == nil {
		globalEnrichment = services.NewEnrichmentService(db)
	}
	if globalEnrichment != nil {
		if artworkDir != "" {
			globalEnrichment.SetArtworkDir(artworkDir)
		}
	}

	srv, err := server.New(server.Config{
		Token:      cfg.AuthToken,
		Port:       0,
		HTTPSPort:  0,
		CertsDir:   filepath.Join(filesDir, "certs"),
		Client:     client,
		DB:         db,
		ChatID:     cfg.ChatID,
		ArtworkDir: artworkDir,
		Enrichment: globalEnrichment,
		Catchup:    globalCatchup,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start server: %w", err)
	}

	if globalEnrichment != nil {
		globalEnrichment.SetChunkReader(srv.Streamer())
		globalEnrichment.SetPlaybackChecker(srv.Streamer().IsPlaybackActive)
		if globalCatchup != nil {
			globalEnrichment.SetSyncChecker(globalCatchup.IsSyncing)
		}
	}

	srv.Start()
	globalServer = srv

	return &StartResult{
		Port:      srv.Port(),
		HTTPSPort: srv.HTTPSPort(),
		Token:     srv.Token(),
	}, nil
}

// Stop shuts down the loopback server, MTProto connection pool, and closes SQLite.
func Stop() {
	globalMu.Lock()
	defer globalMu.Unlock()

	if globalServer != nil {
		_ = globalServer.Close()
		globalServer = nil
	}
	if globalClient != nil {
		globalClient.Stop()
		globalClient = nil
	}
	if globalEnrichment != nil {
		globalEnrichment.Stop()
		globalEnrichment = nil
	}
	if globalDB != nil {
		_ = globalDB.Close()
		globalDB = nil
	}
	if globalWatcher != nil {
		globalWatcher.Stop()
		globalWatcher = nil
	}
	globalCatchup = nil
	globalBackup = nil

	globalCallbackMu.Lock()
	globalCallback = nil
	globalCallbackMu.Unlock()
}

// Health returns a JSON string describing engine status.
func Health() string {
	globalMu.Lock()
	defer globalMu.Unlock()

	if globalServer == nil {
		return `{"status":"stopped"}`
	}
	ready := globalClient != nil && globalClient.IsReady()
	return fmt.Sprintf(`{"status":"running","port":%d,"ready":%t}`, globalServer.Port(), ready)
}

// IsWatcherRunning returns true if the background forum watcher is active.
func IsWatcherRunning() bool {
	globalMu.Lock()
	defer globalMu.Unlock()
	return globalWatcher != nil && globalWatcher.IsRunning()
}

// GetPort returns the bound loopback port, or 0 if stopped.
func GetPort() int {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalServer != nil {
		return globalServer.Port()
	}
	return 0
}

// GetHTTPSPort returns the bound loopback HTTPS port, or 0 if inactive.
func GetHTTPSPort() int {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalServer != nil {
		return globalServer.HTTPSPort()
	}
	return 0
}

// GetToken returns the session authentication token.
func GetToken() string {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalServer != nil {
		return globalServer.Token()
	}
	return ""
}

// GetStreamURL formats the authenticated loopback URL for a track.
func GetStreamURL(trackID string) string {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalServer != nil {
		return fmt.Sprintf("http://127.0.0.1:%d/stream/%s?token=%s", globalServer.Port(), trackID, globalServer.Token())
	}
	return ""
}

// GetTrackSize returns the total file size in bytes for a track, or 0 if unavailable.
func GetTrackSize(trackID string) int64 {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv == nil || srv.Streamer() == nil {
		return 0
	}
	size, err := srv.Streamer().GetTrackSize(context.Background(), trackID)
	if err != nil {
		return 0
	}
	return size
}

// OpenStream opens a dedicated streaming session for trackID, returning a unique handle.
func OpenStream(trackID string) (int64, error) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv == nil || srv.Streamer() == nil {
		return 0, errors.New("engine streamer not initialized")
	}
	return srv.Streamer().OpenStream(context.Background(), trackID)
}

// ReadStream reads up to length bytes at offset from the open session designated by handle.
func ReadStream(handle int64, offset int64, length int) ([]byte, error) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv == nil || srv.Streamer() == nil {
		return nil, errors.New("engine streamer not initialized")
	}
	return srv.Streamer().ReadStream(handle, offset, length)
}

// CloseStream closes the session designated by handle, cancelling outstanding prefetch tasks.
func CloseStream(handle int64) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv != nil && srv.Streamer() != nil {
		srv.Streamer().CloseStream(handle)
	}
}

// ReadTrackChunk reads up to length audio bytes starting at offset for trackID directly.
func ReadTrackChunk(trackID string, offset int64, length int) ([]byte, error) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv == nil || srv.Streamer() == nil {
		return nil, errors.New("engine streamer not initialized")
	}
	return srv.Streamer().ReadChunk(context.Background(), trackID, offset, length)
}

// StartStreamSession initiates an active playback session for trackID.
func StartStreamSession(trackID string) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv != nil && srv.Streamer() != nil {
		srv.Streamer().StartSession(trackID)
	}
}

// ReleaseStreamSession concludes the playback session for trackID.
func ReleaseStreamSession(trackID string) {
	globalMu.Lock()
	srv := globalServer
	globalMu.Unlock()

	if srv != nil && srv.Streamer() != nil {
		srv.Streamer().ReleaseSession(trackID)
	}
}

// SetScreenOn informs the engine whether the device screen is currently on/interactive.
func SetScreenOn(on bool) {
}

// GetArtworkDirName returns the shared directory name for cover artwork ("artwork").
func GetArtworkDirName() string {
	return metadata.ArtworkDirName
}

// GetEnrichmentSummary returns the summary line with library counts:
// [ENRICH_SUMMARY] total=%d embedded_ok=%d no_art=%d transient_fails=%d permanent_fails=%d online_matched=%d pending=%d
func GetEnrichmentSummary() string {
	globalMu.Lock()
	db := globalDB
	globalMu.Unlock()

	if db == nil {
		return "[ENRICH_SUMMARY] database not open"
	}
	return db.GetEnrichmentSummaryLine(context.Background())
}

// GetDiagnosticsClipboard returns the formatted diagnostic report.
func GetDiagnosticsClipboard(statsLimit, enrichLimit int) string {
	return GetEnrichmentSummary()
}

// GetStreamingStats returns the recent streaming stats lines.
func GetStreamingStats(limit int) string {
	return ""
}

// GetEnrichmentLogs returns the recent enrichment decision JSON lines.
func GetEnrichmentLogs(limit int) string {
	return ""
}


// SyncChannel triggers incremental catch-up and message scanning for a Telegram channel.
func SyncChannel(chatID int64) error {
	globalMu.Lock()
	syncer := globalCatchup
	globalMu.Unlock()

	if syncer == nil {
		err := errors.New("catch-up service is not initialized or telegram pool is not configured")
		notifyError(err.Error())
		return err
	}
	notifyProgress("sync_started", 0, 100)
	err := syncer.SyncChannel(context.Background(), chatID)
	if err != nil {
		notifyError(err.Error())
		return err
	}
	notifyProgress("sync_complete", 100, 100)
	return nil
}

// RescanFromScratch clears the SQLite database and rescans the group and all its topics from message 1.
func RescanFromScratch(chatID int64) error {
	globalMu.Lock()
	syncer := globalCatchup
	globalMu.Unlock()

	if syncer == nil {
		err := errors.New("catch-up service is not initialized or telegram pool is not configured")
		notifyError(err.Error())
		return err
	}
	notifyProgress("rescan_started", 0, 100)
	err := syncer.RescanFromScratch(context.Background(), chatID)
	if err != nil {
		notifyError(err.Error())
		return err
	}
	notifyProgress("rescan_complete", 100, 100)
	return nil
}

// BackupDatabase exports a clean SQLite vacuum snapshot and uploads it to the Telegram System topic.
func BackupDatabase(chatID int64, topicID int) error {
	globalMu.Lock()
	backup := globalBackup
	globalMu.Unlock()

	if backup == nil {
		err := errors.New("backup service is not initialized")
		notifyError(err.Error())
		return err
	}
	notifyProgress("backup_started", 0, 100)
	_, err := backup.Backup(context.Background(), chatID, int32(topicID))
	if err != nil {
		notifyError(err.Error())
		return err
	}
	notifyProgress("backup_complete", 100, 100)
	return nil
}

// RestoreDatabase downloads and applies the latest SQLite snapshot from the Telegram System topic.
func RestoreDatabase(chatID int64, topicID int) error {
	globalMu.Lock()
	backup := globalBackup
	globalMu.Unlock()

	if backup == nil {
		err := errors.New("backup service is not initialized")
		notifyError(err.Error())
		return err
	}
	notifyProgress("restore_started", 0, 100)
	err := backup.Restore(context.Background(), chatID, int32(topicID))
	if err != nil {
		notifyError(err.Error())
		return err
	}
	notifyProgress("restore_complete", 100, 100)
	return nil
}

// RegisterMockTrack registers a mock track for offline verification and benchmarks.
func RegisterMockTrack(trackID, title, artist string, durationSec int, mimeType string, data []byte) {
	globalMu.Lock()
	defer globalMu.Unlock()

	if globalClient != nil {
		track := &models.Track{
			ID:          trackID,
			Title:       title,
			Artist:      artist,
			DurationSec: durationSec,
			MimeType:    mimeType,
			FileSize:    int64(len(data)),
			FileName:    fmt.Sprintf("%s.mp3", trackID),
		}
		if mock, ok := globalClient.(*telegram.MockClient); ok {
			mock.RegisterTrackWithPayload(track, data)
		} else {
			globalClient.RegisterTrack(track)
		}
	}
}
