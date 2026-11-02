package models

import "time"

// Track represents an audio track canonical record.
type Track struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Artist      string    `json:"artist"`
	Album       string    `json:"album"`
	DurationSec int       `json:"duration_sec"`
	MimeType    string    `json:"mime_type"`
	FileSize    int64     `json:"file_size"`
	FileName    string    `json:"file_name"`
	CreatedAt   time.Time `json:"created_at"`

	// Enrichment and Deduplication metadata
	Fingerprint  string `json:"fingerprint,omitempty"`
	CoverURL     string `json:"cover_url,omitempty"`
	ThumbURL     string `json:"thumb_url,omitempty"`
	Lyrics       string `json:"lyrics,omitempty"`
	SyncedLyrics string `json:"synced_lyrics,omitempty"`
	Year         int    `json:"year,omitempty"`

	// Primary Telegram source reference
	FileID        string `json:"file_id,omitempty"`
	ChatID        int64  `json:"chat_id,omitempty"`
	MsgID         int32  `json:"msg_id,omitempty"`
	AccessHash    int64  `json:"access_hash,omitempty"`
	FileReference []byte `json:"file_reference,omitempty"`
}

// TrackSource links a track to a specific message in a channel/chat.
// One Track can have multiple sources (e.g. forwarded into multiple topic playlists).
type TrackSource struct {
	TrackID      string    `json:"track_id"`
	ChatID       int64     `json:"chat_id"`
	MsgID        int32     `json:"msg_id"`
	TopicID      int32     `json:"topic_id"`
	FileUniqueID string    `json:"file_unique_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// Topic represents a Telegram Forum Topic / Playlist Folder.
type Topic struct {
	ID          int32     `json:"id"`
	ChatID      int64     `json:"chat_id"`
	Name        string    `json:"name"`
	TrackCount  int       `json:"track_count"`
	IconColor   int       `json:"icon_color,omitempty"`
	IconEmojiID int64     `json:"icon_emoji_id,omitempty"`
	IsFolder    bool      `json:"is_folder"`
	ArtworkURL  string    `json:"artwork_url,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Playlist represents a user or topic playlist.
type Playlist struct {
	ID             string    `json:"id"`
	ChatID         int64     `json:"chat_id"`
	TopicID        int32     `json:"topic_id"`
	Title          string    `json:"title"`
	Subtitle       string    `json:"subtitle"`
	ArtworkURL     string    `json:"artwork_url,omitempty"`
	IsFolder       bool      `json:"is_folder"`
	ParentFolderID string    `json:"parent_folder_id,omitempty"`
	TrackCount     int       `json:"track_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// PlaylistTrack links a track to a playlist with ordered position.
type PlaylistTrack struct {
	PlaylistID string    `json:"playlist_id"`
	TrackID    string    `json:"track_id"`
	Position   int       `json:"position"`
	AddedAt    time.Time `json:"added_at"`
}

// BackupRecord tracks vacuum snapshots uploaded or discovered in Telegram topics.
type BackupRecord struct {
	MsgID         int32     `json:"msg_id"`
	ChatID        int64     `json:"chat_id"`
	TopicID       int32     `json:"topic_id"`
	DocID         int64     `json:"doc_id"`
	AccessHash    int64     `json:"access_hash"`
	FileReference []byte    `json:"file_reference,omitempty"`
	Caption       string    `json:"caption,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Artist represents an aggregated artist entity.
type Artist struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	TrackCount int      `json:"track_count"`
	Albums     []string `json:"albums,omitempty"`
	ThumbURL   string   `json:"thumb_url,omitempty"`
}

// Album represents an aggregated album entity.
type Album struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Year       int    `json:"year"`
	TrackCount int    `json:"track_count"`
	CoverURL   string `json:"cover_url,omitempty"`
}

// WorkerLocation holds per-bot session-specific access hash & file reference for a message.
type WorkerLocation struct {
	BotID         string    `json:"bot_id"`
	MsgID         int32     `json:"msg_id"`
	DocID         int64     `json:"doc_id"`
	AccessHash    int64     `json:"access_hash"`
	FileReference []byte    `json:"file_reference"`
	DCID          int32     `json:"dc_id"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SyncState tracks gotd update state per channel for incremental catch-up.
type SyncState struct {
	ChannelID int64     `json:"channel_id"`
	Pts       int       `json:"pts"`
	Date      int       `json:"date"`
	Qts       int       `json:"qts"`
	MaxMsgID  int32     `json:"max_msg_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Config represents runtime configuration passed from Android.
// Note: Tokens and secrets must never be exported to the backed-up database.
type Config struct {
	ApiID         int      `json:"api_id"`
	ApiHash       string   `json:"api_hash"`
	BotToken      string   `json:"bot_token"`                  // Primary bot token
	WorkerTokens  []string `json:"worker_tokens"`              // N-1 worker bot tokens
	BotTokens     []string `json:"bot_tokens,omitempty"`       // Raw tokens list from Android
	WorkerCount   int      `json:"worker_count"`               // Total bot pool size (default: 3)
	ChatID        int64    `json:"chat_id"`                    // Target music group / channel
	SystemTopicID int32    `json:"system_topic_id"`            // Topic ID for DB backup & sync
	FilesDir      string   `json:"files_dir"`
	CacheDir      string   `json:"cache_dir"`
	AuthToken     string   `json:"auth_token,omitempty"`
}

