package config

import (
	"time"
)

// Network and server configurations
const (
	DefaultDomain        = "local.burhanverse.eu.org"
	DefaultLoopbackHost  = "127.0.0.1"
	HTTPReadTimeout      = 30 * time.Second
	HTTPIdleTimeout      = 60 * time.Second
)

// Audio streaming and buffer configurations
const (
	DefaultChunkSize      = 8 * 1024 * 1024 // chunk size
	InitialSmallChunkSize = 512 * 1024      // initial chunk for fast TTFB
	FullChunkSize         = 8 * 1024 * 1024 // full chunk
	MinReadAheadBytes     = 4 * 1024 * 1024 // minimum read-ahead prefetch
	MaxReadAheadBytes     = 8 * 1024 * 1024 // maximum read-ahead prefetch window
	DefaultBitrateBps     = 32000           // fallback audio bitrate (32 KB/s)
	DefaultReadAheadSec   = 30              // Target read-ahead buffer in seconds
	PrefetchCacheCapacity = 8               // prefetch cache capacity
	BgCacheCapacity       = 8               // background cache capacity
	MaxBgBlockSize        = 512 * 1024      // range reader block size
)

// Telegram MTProto protocol constraints
const (
	MTProtoChunkLimit      = 1024 * 1024 // 1 MB maximum limit per UploadGetFile RPC
	MTProtoOffsetAlignment = 4096        // 4 KB boundary alignment
)

// Artwork and media limits
const (
	ArtworkDirName     = "artwork"
	MaxArtworkFileSize = 8 * 1024 * 1024 // 8 MB max image size before skipping
	ArtworkTargetSize  = 600             // 600x600 px target downscale resolution
	ArtworkJPEGQuality = 100             // JPEG compression quality
)

// Enrichment service configurations
const (
	EnrichmentBatchInterval = 10 * time.Second
	EnrichmentMaxConcurrent = 2
	EnrichmentCountry       = "in"
)

// Config specifies credentials and operational settings for the engine.
type Config struct {
	ApiID         int      `json:"api_id"`
	ApiHash       string   `json:"api_hash"`
	BotToken      string   `json:"bot_token"`
	WorkerTokens  []string `json:"worker_tokens"`
	BotTokens     []string `json:"bot_tokens,omitempty"`
	WorkerCount   int      `json:"worker_count"`
	ChatID        int64    `json:"chat_id"`
	SystemTopicID int32    `json:"system_topic_id"`
	FilesDir      string   `json:"files_dir"`
	CacheDir      string   `json:"cache_dir"`
	AuthToken     string   `json:"auth_token,omitempty"`
	Debug         bool     `json:"debug,omitempty"`
}
