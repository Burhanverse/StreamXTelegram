package models

import (
	"context"
	"time"

	"musix/engine/internal/config"
)

type ContextKey string

const (
	ContextKeyTrackID    ContextKey = "stream_track_id"
	ContextKeyBackground ContextKey = "stream_is_background"
	ContextKeyPriority   ContextKey = "stream_priority"
)

// Priority represents the priority level of a bot network request.
type Priority int

const (
	PriorityBackground Priority = 0
	PriorityPrefetch   Priority = 1
	PriorityPlayback   Priority = 2
)

// WithPriority decorates ctx with the given request priority.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, ContextKeyPriority, p)
}

// PriorityFromContext retrieves the priority from ctx, defaulting to PriorityPlayback
// unless marked as background.
func PriorityFromContext(ctx context.Context) Priority {
	if val, ok := ctx.Value(ContextKeyPriority).(Priority); ok {
		return val
	}
	if IsBackgroundContext(ctx) {
		return PriorityBackground
	}
	return PriorityPlayback
}

// WithTrackID decorates ctx with the active trackID for telemetry tracking.
func WithTrackID(ctx context.Context, trackID string) context.Context {
	return context.WithValue(ctx, ContextKeyTrackID, trackID)
}

// TrackIDFromContext retrieves the trackID from ctx if present.
func TrackIDFromContext(ctx context.Context) string {
	if val, ok := ctx.Value(ContextKeyTrackID).(string); ok {
		return val
	}
	return ""
}

// WithBackground marks ctx as a background/enrichment operation.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, ContextKeyBackground, true)
}

// IsBackgroundContext checks if ctx was marked as a background operation.
func IsBackgroundContext(ctx context.Context) bool {
	if val, ok := ctx.Value(ContextKeyBackground).(bool); ok {
		return val
	}
	return false
}

// Track represents an audio track canonical record.
type Track struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Artist      string    `json:"artist"`
	Artists     []string  `json:"artists,omitempty"`
	Album       string    `json:"album"`
	AlbumArtist string    `json:"album_artist,omitempty"`
	TrackNumber int       `json:"track_number,omitempty"`
	DiscNumber  int       `json:"disc_number,omitempty"`
	Genre       string    `json:"genre,omitempty"`
	Composer    string    `json:"composer,omitempty"`
	DurationSec int       `json:"duration_sec"`
	SampleRate  int       `json:"sample_rate,omitempty"`
	MimeType    string    `json:"mime_type"`
	FileSize    int64     `json:"file_size"`
	FileName    string    `json:"file_name"`
	CreatedAt   time.Time `json:"created_at"`

	// Enrichment and Deduplication metadata
	Fingerprint  string    `json:"fingerprint,omitempty"`
	ThumbnailURL string    `json:"thumbnail_url,omitempty"`
	CoverURL     string    `json:"cover_url,omitempty"`
	Lyrics       string    `json:"lyrics,omitempty"`
	SyncedLyrics string    `json:"synced_lyrics,omitempty"`
	Year         int       `json:"year,omitempty"`
	MatchSource  string    `json:"match_source,omitempty"`
	MatchScore   float64   `json:"match_score,omitempty"`
	MatchedAt    time.Time `json:"matched_at,omitempty"`
	MetaVersion  int       `json:"meta_version,omitempty"`

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
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	TrackCount   int      `json:"track_count"`
	Albums       []string `json:"albums,omitempty"`
	ThumbnailURL string   `json:"thumbnail_url,omitempty"`
}

// Album represents an aggregated album entity.
type Album struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Artist       string `json:"artist"`
	Year         int    `json:"year"`
	TrackCount   int    `json:"track_count"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	CoverURL     string `json:"cover_url,omitempty"`
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
type Config = config.Config

