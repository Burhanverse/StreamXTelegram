package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"musix/engine/internal/models"
)

const CurrentSchemaVersion = 4

// DB encapsulates the embedded SQLite database for on-device metadata.
type DB struct {
	db   *sql.DB
	path string
	mu   sync.RWMutex
}

// Open initializes SQLite in WAL mode and runs pending migrations.
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Single connection writer limit to prevent SQLite lock contention on Android
	sqlDB.SetMaxOpenConns(1)

	db := &DB{
		db:   sqlDB,
		path: dbPath,
	}

	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("database migration failed: %w", err)
	}

	return db, nil
}

// Path returns the SQLite database file path.
func (d *DB) Path() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.path
}

// Close closes the SQLite connection.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

func (d *DB) migrate() error {
	ctx := context.Background()

	_, err := d.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create schema_migrations: %w", err)
	}

	var version int
	row := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	if err := row.Scan(&version); err != nil {
		return fmt.Errorf("failed to query schema version: %w", err)
	}

	if version < 1 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// 1. Sync state for gotd updates catch-up
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS sync_state (
				channel_id INTEGER PRIMARY KEY,
				pts INTEGER NOT NULL DEFAULT 0,
				date INTEGER NOT NULL DEFAULT 0,
				qts INTEGER NOT NULL DEFAULT 0,
				max_msg_id INTEGER NOT NULL DEFAULT 0,
				updated_at DATETIME NOT NULL
			);
		`); err != nil {
			return err
		}

		// 2. Canonical tracks table
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS tracks (
				id TEXT PRIMARY KEY,
				title TEXT NOT NULL,
				artist TEXT NOT NULL,
				album TEXT NOT NULL,
				duration_sec INTEGER NOT NULL,
				mime_type TEXT NOT NULL,
				file_size INTEGER NOT NULL,
				file_name TEXT NOT NULL,
				created_at DATETIME NOT NULL
			);
			CREATE INDEX IF NOT EXISTS idx_tracks_artist ON tracks(artist);
			CREATE INDEX IF NOT EXISTS idx_tracks_album ON tracks(album);
		`); err != nil {
			return err
		}

		// 3. Track sources (one track -> many chat_id/msg_id mappings)
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS track_sources (
				track_id TEXT NOT NULL,
				chat_id INTEGER NOT NULL,
				msg_id INTEGER NOT NULL,
				topic_id INTEGER NOT NULL DEFAULT 0,
				file_unique_id TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY(track_id, chat_id, msg_id),
				FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);
			CREATE INDEX IF NOT EXISTS idx_sources_msg ON track_sources(chat_id, msg_id);
			CREATE INDEX IF NOT EXISTS idx_sources_topic ON track_sources(topic_id);
			CREATE INDEX IF NOT EXISTS idx_sources_file_unique ON track_sources(file_unique_id);
		`); err != nil {
			return err
		}

		// 4. Per-bot cached file locations ({bot_id, msg_id} -> {doc_id, access_hash, file_reference})
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS worker_file_refs (
				bot_id TEXT NOT NULL,
				msg_id INTEGER NOT NULL,
				doc_id INTEGER NOT NULL DEFAULT 0,
				access_hash INTEGER NOT NULL,
				file_reference BLOB NOT NULL,
				dc_id INTEGER NOT NULL DEFAULT 2,
				updated_at DATETIME NOT NULL,
				PRIMARY KEY(bot_id, msg_id)
			);
		`); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (1, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if version < 2 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		alterStmts := []string{
			`ALTER TABLE tracks ADD COLUMN fingerprint TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN cover_url TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN thumb_url TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN lyrics TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN synced_lyrics TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN year INTEGER DEFAULT 0;`,
			`CREATE INDEX IF NOT EXISTS idx_tracks_fingerprint ON tracks(fingerprint);`,
			`CREATE TABLE IF NOT EXISTS topics (
				id INTEGER NOT NULL,
				chat_id INTEGER NOT NULL,
				name TEXT NOT NULL,
				track_count INTEGER DEFAULT 0,
				updated_at DATETIME NOT NULL,
				PRIMARY KEY(chat_id, id)
			);`,
		}
		for _, stmt := range alterStmts {
			_, _ = tx.ExecContext(ctx, stmt)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (2, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if version < 3 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		_, _ = tx.ExecContext(ctx, `ALTER TABLE worker_file_refs ADD COLUMN doc_id INTEGER DEFAULT 0;`)
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (3, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if version < 4 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		alterStmts := []string{
			`ALTER TABLE topics ADD COLUMN icon_color INTEGER DEFAULT 0;`,
			`ALTER TABLE topics ADD COLUMN icon_emoji_id INTEGER DEFAULT 0;`,
			`ALTER TABLE topics ADD COLUMN is_folder BOOLEAN DEFAULT 1;`,
			`CREATE TABLE IF NOT EXISTS playlists (
				id TEXT PRIMARY KEY,
				chat_id INTEGER NOT NULL DEFAULT 0,
				topic_id INTEGER NOT NULL DEFAULT 0,
				title TEXT NOT NULL,
				subtitle TEXT DEFAULT '',
				artwork_url TEXT DEFAULT '',
				is_folder BOOLEAN DEFAULT 0,
				parent_folder_id TEXT DEFAULT '',
				track_count INTEGER DEFAULT 0,
				created_at DATETIME NOT NULL,
				updated_at DATETIME NOT NULL
			);`,
			`CREATE INDEX IF NOT EXISTS idx_playlists_topic ON playlists(topic_id);`,
			`CREATE INDEX IF NOT EXISTS idx_playlists_parent ON playlists(parent_folder_id);`,
			`CREATE TABLE IF NOT EXISTS playlist_tracks (
				playlist_id TEXT NOT NULL,
				track_id TEXT NOT NULL,
				position INTEGER NOT NULL,
				added_at DATETIME NOT NULL,
				PRIMARY KEY (playlist_id, track_id),
				FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
				FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);`,
			`CREATE INDEX IF NOT EXISTS idx_pl_tracks_pos ON playlist_tracks(playlist_id, position);`,
			`CREATE TABLE IF NOT EXISTS backups (
				msg_id INTEGER PRIMARY KEY,
				chat_id INTEGER NOT NULL,
				topic_id INTEGER NOT NULL,
				doc_id INTEGER NOT NULL DEFAULT 0,
				access_hash INTEGER NOT NULL DEFAULT 0,
				file_reference BLOB,
				caption TEXT DEFAULT '',
				created_at DATETIME NOT NULL
			);`,
			`CREATE INDEX IF NOT EXISTS idx_backups_topic ON backups(chat_id, topic_id);`,
		}
		for _, stmt := range alterStmts {
			_, _ = tx.ExecContext(ctx, stmt)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (4, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if version < 5 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		_, err = tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS enrichment_state (
				track_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				attempts INTEGER NOT NULL DEFAULT 0,
				last_attempt DATETIME NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				expires_at DATETIME NOT NULL,
				FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);
			CREATE INDEX IF NOT EXISTS idx_enrichment_state_status ON enrichment_state(status, expires_at);
		`)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (5, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if version < 6 {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		alterStmts := []string{
			`ALTER TABLE tracks ADD COLUMN match_source TEXT DEFAULT '';`,
			`ALTER TABLE tracks ADD COLUMN match_score REAL DEFAULT 0.0;`,
			`ALTER TABLE tracks ADD COLUMN matched_at DATETIME;`,
			`ALTER TABLE tracks ADD COLUMN meta_version INTEGER DEFAULT 2;`,
		}
		for _, stmt := range alterStmts {
			_, _ = tx.ExecContext(ctx, stmt)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (6, ?);`, time.Now()); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

// SaveTrack inserts or updates a canonical track.
func (d *DB) SaveTrack(ctx context.Context, t *models.Track) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tracks (id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at, fingerprint, cover_url, thumb_url, lyrics, synced_lyrics, year)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			artist = excluded.artist,
			album = excluded.album,
			duration_sec = excluded.duration_sec,
			mime_type = excluded.mime_type,
			file_size = excluded.file_size,
			file_name = excluded.file_name,
			fingerprint = CASE WHEN excluded.fingerprint != '' THEN excluded.fingerprint ELSE tracks.fingerprint END,
			cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
			thumb_url = CASE WHEN excluded.thumb_url != '' THEN excluded.thumb_url ELSE tracks.thumb_url END,
			lyrics = CASE WHEN excluded.lyrics != '' THEN excluded.lyrics ELSE tracks.lyrics END,
			synced_lyrics = CASE WHEN excluded.synced_lyrics != '' THEN excluded.synced_lyrics ELSE tracks.synced_lyrics END,
			year = CASE WHEN excluded.year > 0 THEN excluded.year ELSE tracks.year END;
	`, t.ID, t.Title, t.Artist, t.Album, t.DurationSec, t.MimeType, t.FileSize, t.FileName, t.CreatedAt,
		t.Fingerprint, t.CoverURL, t.ThumbURL, t.Lyrics, t.SyncedLyrics, t.Year)
	return err
}

// SaveTracksBatch inserts a slice of tracks within a single SQLite transaction.
func (d *DB) SaveTracksBatch(ctx context.Context, tracks []*models.Track) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO tracks (id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at, fingerprint, cover_url, thumb_url, lyrics, synced_lyrics, year)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			artist = excluded.artist,
			album = excluded.album,
			duration_sec = excluded.duration_sec,
			mime_type = excluded.mime_type,
			file_size = excluded.file_size,
			file_name = excluded.file_name,
			fingerprint = CASE WHEN excluded.fingerprint != '' THEN excluded.fingerprint ELSE tracks.fingerprint END,
			cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
			thumb_url = CASE WHEN excluded.thumb_url != '' THEN excluded.thumb_url ELSE tracks.thumb_url END,
			lyrics = CASE WHEN excluded.lyrics != '' THEN excluded.lyrics ELSE tracks.lyrics END,
			synced_lyrics = CASE WHEN excluded.synced_lyrics != '' THEN excluded.synced_lyrics ELSE tracks.synced_lyrics END,
			year = CASE WHEN excluded.year > 0 THEN excluded.year ELSE tracks.year END;
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	for _, t := range tracks {
		createdAt := t.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		_, err := stmt.ExecContext(ctx,
			t.ID, t.Title, t.Artist, t.Album, t.DurationSec, t.MimeType, t.FileSize, t.FileName,
			createdAt, t.Fingerprint, t.CoverURL, t.ThumbURL, t.Lyrics, t.SyncedLyrics, t.Year,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetTrack retrieves a track by its canonical ID.
func (d *DB) GetTrack(ctx context.Context, id string) (*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.db.QueryRowContext(ctx, `
		SELECT t.id, t.title, t.artist, t.album, t.duration_sec, t.mime_type, t.file_size, t.file_name, t.created_at,
		       COALESCE(t.fingerprint, ''), COALESCE(t.cover_url, ''), COALESCE(t.thumb_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.year, 0),
		       COALESCE(s.chat_id, 0), COALESCE(s.msg_id, 0)
		FROM tracks t
		LEFT JOIN track_sources s ON t.id = s.track_id
		WHERE t.id = ?
		LIMIT 1
	`, id)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year,
		&t.ChatID, &t.MsgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpdateTrackEnrichment updates external metadata fields on an existing track record.
func (d *DB) UpdateTrackEnrichment(ctx context.Context, id, coverURL, thumbURL, lyrics, syncedLyrics string, year int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
			thumb_url = CASE WHEN ? != '' THEN ? ELSE thumb_url END,
			lyrics = CASE WHEN ? != '' THEN ? ELSE lyrics END,
			synced_lyrics = CASE WHEN ? != '' THEN ? ELSE synced_lyrics END,
			year = CASE WHEN ? > 0 THEN ? ELSE year END
		WHERE id = ?;
	`, coverURL, coverURL, thumbURL, thumbURL, lyrics, lyrics, syncedLyrics, syncedLyrics, year, year, id)
	return err
}

// UpdateTrackEnrichmentWithAlbum updates external metadata fields including album on an existing track record.
func (d *DB) UpdateTrackEnrichmentWithAlbum(ctx context.Context, id, album, coverURL, thumbURL, lyrics, syncedLyrics string, year int) error {
	return d.UpdateTrackFullMetadata(ctx, id, "", "", album, coverURL, thumbURL, lyrics, syncedLyrics, year)
}

// UpdateTrackFullMetadata updates title, artist, album, cover_url, thumb_url, lyrics, synced_lyrics, and year.
func (d *DB) UpdateTrackFullMetadata(ctx context.Context, id, title, artist, album, coverURL, thumbURL, lyrics, syncedLyrics string, year int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			title = CASE WHEN ? != '' THEN ? ELSE title END,
			artist = CASE WHEN ? != '' AND ? != 'Unknown Artist' THEN ? ELSE artist END,
			album = CASE WHEN ? != '' AND ? != 'Telegram Music' THEN ? ELSE album END,
			cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
			thumb_url = CASE WHEN ? != '' THEN ? ELSE thumb_url END,
			lyrics = CASE WHEN ? != '' THEN ? ELSE lyrics END,
			synced_lyrics = CASE WHEN ? != '' THEN ? ELSE synced_lyrics END,
			year = CASE WHEN ? > 0 THEN ? ELSE year END
		WHERE id = ?;
	`, title, title, artist, artist, artist, album, album, album, coverURL, coverURL, thumbURL, thumbURL, lyrics, lyrics, syncedLyrics, syncedLyrics, year, year, id)
	return err
}

// UpdateTrackMatchMetadata persists the matching provenance, score, and metadata schema version.
func (d *DB) UpdateTrackMatchMetadata(ctx context.Context, id, matchSource string, matchScore float64, metaVersion int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			match_source = ?,
			match_score = ?,
			matched_at = ?,
			meta_version = ?
		WHERE id = ?;
	`, matchSource, matchScore, time.Now(), metaVersion, id)
	return err
}

// SaveEnrichmentState updates the persistent enrichment status, attempt count, and negative cache TTL.
func (d *DB) SaveEnrichmentState(ctx context.Context, trackID string, status string, attempts int, reason string, ttl time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	expiresAt := time.Now().Add(ttl)
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO enrichment_state (track_id, status, attempts, last_attempt, reason, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(track_id) DO UPDATE SET
			status = excluded.status,
			attempts = attempts + 1,
			last_attempt = excluded.last_attempt,
			reason = excluded.reason,
			expires_at = excluded.expires_at;
	`, trackID, status, attempts, time.Now(), reason, expiresAt)
	return err
}

// IsEnrichmentNegativeCached returns true if trackID previously failed and its negative cache TTL is still active.
func (d *DB) IsEnrichmentNegativeCached(ctx context.Context, trackID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var count int
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM enrichment_state
		WHERE track_id = ? AND status = 'failed' AND expires_at > ?;
	`, trackID, time.Now()).Scan(&count)
	return err == nil && count > 0
}

// ListUnenrichedTrackIDs returns track IDs that have no valid cover_url, have low match score,
// or have default album/artist names, respecting negative cache TTL and capping batch size.
func (d *DB) ListUnenrichedTrackIDs(ctx context.Context) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id FROM tracks t
		LEFT JOIN enrichment_state e ON t.id = e.track_id
		WHERE (
			t.cover_url IS NULL 
			OR t.cover_url = '' 
			OR (t.meta_version < 2 AND NOT t.cover_url LIKE 'content://%')
			OR (t.match_score < 0.70 AND NOT t.cover_url LIKE 'content://%')
			OR t.thumb_url IS NULL 
			OR t.thumb_url = '' 
			OR t.album = 'Telegram Music' 
			OR t.album = ''
			OR t.artist = 'Unknown Artist'
			OR t.artist = ''
		)
		AND (e.track_id IS NULL OR (e.status != 'success' AND e.expires_at <= ?))
		ORDER BY t.created_at DESC
		LIMIT 100;
	`, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// ResetAllTables clears all tracks, sources, worker file references, topics, playlists, and sync state.
func (d *DB) ResetAllTables(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tables := []string{
		"playlist_tracks",
		"playlists",
		"track_sources",
		"worker_file_refs",
		"tracks",
		"topics",
		"sync_state",
	}

	for _, tbl := range tables {
		if _, err := d.db.ExecContext(ctx, "DELETE FROM "+tbl+";"); err != nil {
			return fmt.Errorf("failed to clear table %s: %w", tbl, err)
		}
	}
	_, _ = d.db.ExecContext(ctx, "VACUUM;")
	return nil
}

// FindTrackByFingerprint retrieves an existing track with a matching metadata fingerprint.
func (d *DB) FindTrackByFingerprint(ctx context.Context, fingerprint string) (*models.Track, error) {
	if fingerprint == "" {
		return nil, nil
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.db.QueryRowContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks WHERE fingerprint = ? LIMIT 1
	`, fingerprint)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// FindTrackByFuzzy looks for a match based on title, artist, and duration tolerance (+-2 seconds).
func (d *DB) FindTrackByFuzzy(ctx context.Context, title, artist string, durationSec int) (*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	query := `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		WHERE LOWER(title) = LOWER(?) AND LOWER(artist) = LOWER(?)
	`
	args := []any{title, artist}
	if durationSec > 0 {
		query += ` AND ABS(duration_sec - ?) <= 2`
		args = append(args, durationSec)
	}
	query += ` LIMIT 1`

	row := d.db.QueryRowContext(ctx, query, args...)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// FindTrackByFileUniqueID searches for a track already indexed with this Telegram document ID.
func (d *DB) FindTrackByFileUniqueID(ctx context.Context, fileUniqueID string) (*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.db.QueryRowContext(ctx, `
		SELECT t.id, t.title, t.artist, t.album, t.duration_sec, t.mime_type, t.file_size, t.file_name, t.created_at,
		       COALESCE(t.fingerprint, ''), COALESCE(t.cover_url, ''), COALESCE(t.thumb_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.year, 0)
		FROM track_sources s
		JOIN tracks t ON s.track_id = t.id
		WHERE s.file_unique_id = ? LIMIT 1
	`, fileUniqueID)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// AddTrackSource associates a chat/message/topic source with a track.
func (d *DB) AddTrackSource(ctx context.Context, src *models.TrackSource) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if src.CreatedAt.IsZero() {
		src.CreatedAt = time.Now()
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO track_sources (track_id, chat_id, msg_id, topic_id, file_unique_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(track_id, chat_id, msg_id) DO UPDATE SET
			topic_id = excluded.topic_id,
			file_unique_id = excluded.file_unique_id;
	`, src.TrackID, src.ChatID, src.MsgID, src.TopicID, src.FileUniqueID, src.CreatedAt)
	return err
}

// GetTrackSources returns all sources for a track.
func (d *DB) GetTrackSources(ctx context.Context, trackID string) ([]*models.TrackSource, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT track_id, chat_id, msg_id, topic_id, file_unique_id, created_at
		FROM track_sources WHERE track_id = ?
	`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []*models.TrackSource
	for rows.Next() {
		var s models.TrackSource
		if err := rows.Scan(&s.TrackID, &s.ChatID, &s.MsgID, &s.TopicID, &s.FileUniqueID, &s.CreatedAt); err != nil {
			return nil, err
		}
		sources = append(sources, &s)
	}
	return sources, rows.Err()
}

// NormalizeChatID returns the normalized positive channel ID (without -100 prefix)
// and the alternative representation for dual-lookup compatibility.
func NormalizeChatID(chatID int64) (norm int64, alt int64) {
	if chatID <= -1000000000000 {
		return -chatID - 1000000000000, chatID
	}
	if chatID > 1000000000 {
		return chatID, -(chatID + 1000000000000)
	}
	return chatID, chatID
}

// SaveTopic upserts a forum topic playlist entity.
func (d *DB) SaveTopic(ctx context.Context, topic *models.Topic) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	norm, _ := NormalizeChatID(topic.ChatID)
	topic.ChatID = norm

	isFolderInt := 1
	if !topic.IsFolder {
		isFolderInt = 0
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO topics (id, chat_id, name, track_count, icon_color, icon_emoji_id, is_folder, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, id) DO UPDATE SET
			name = excluded.name,
			track_count = CASE WHEN excluded.track_count > 0 THEN excluded.track_count ELSE topics.track_count END,
			icon_color = excluded.icon_color,
			icon_emoji_id = excluded.icon_emoji_id,
			is_folder = excluded.is_folder,
			updated_at = excluded.updated_at;
	`, topic.ID, topic.ChatID, topic.Name, topic.TrackCount, topic.IconColor, topic.IconEmojiID, isFolderInt, time.Now())
	return err
}

// GetTopic fetches a single topic by ID with track count and artwork.
func (d *DB) GetTopic(ctx context.Context, chatID int64, topicID int32) (*models.Topic, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	norm, alt := NormalizeChatID(chatID)
	var t models.Topic
	var isFolder int
	err := d.db.QueryRowContext(ctx, `
		SELECT tp.id, tp.chat_id, tp.name, COUNT(DISTINCT s.track_id),
		       COALESCE(tp.icon_color, 0), COALESCE(tp.icon_emoji_id, 0), COALESCE(tp.is_folder, 1),
		       COALESCE(NULLIF(MAX(t.cover_url), ''), MAX(t.thumb_url), ''),
		       tp.updated_at
		FROM topics tp
		LEFT JOIN track_sources s ON (tp.chat_id = s.chat_id OR s.chat_id = ? OR s.chat_id = ?) AND tp.id = s.topic_id
		LEFT JOIN tracks t ON s.track_id = t.id
		WHERE (tp.chat_id = ? OR tp.chat_id = ?) AND tp.id = ?
		GROUP BY tp.id, tp.chat_id
	`, norm, alt, norm, alt, topicID).Scan(&t.ID, &t.ChatID, &t.Name, &t.TrackCount, &t.IconColor, &t.IconEmojiID, &isFolder, &t.ArtworkURL, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Fallback to track_sources if topic not yet explicitly in topics table
			errFallback := d.db.QueryRowContext(ctx, `
				SELECT s.topic_id, s.chat_id, CAST(s.topic_id AS TEXT), COUNT(DISTINCT s.track_id), 0, 0, 1,
				       COALESCE(NULLIF(MAX(t.cover_url), ''), MAX(t.thumb_url), '')
				FROM track_sources s
				LEFT JOIN tracks t ON s.track_id = t.id
				WHERE (s.chat_id = ? OR s.chat_id = ?) AND s.topic_id = ?
				GROUP BY s.topic_id, s.chat_id
			`, norm, alt, topicID).Scan(&t.ID, &t.ChatID, &t.Name, &t.TrackCount, &t.IconColor, &t.IconEmojiID, &isFolder, &t.ArtworkURL)
			if errFallback != nil {
				return nil, nil
			}
			t.IsFolder = true
			t.UpdatedAt = time.Now()
			return &t, nil
		}
		return nil, err
	}
	t.IsFolder = isFolder != 0
	return &t, nil
}

// ListTopics returns all forum topic playlist folders with active track counts and artwork.
func (d *DB) ListTopics(ctx context.Context, chatID int64) ([]*models.Topic, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	norm, alt := NormalizeChatID(chatID)
	rows, err := d.db.QueryContext(ctx, `
		SELECT tp.id, tp.chat_id, tp.name, COUNT(DISTINCT s.track_id),
		       COALESCE(tp.icon_color, 0), COALESCE(tp.icon_emoji_id, 0), COALESCE(tp.is_folder, 1),
		       COALESCE(NULLIF(MAX(t.cover_url), ''), MAX(t.thumb_url), '')
		FROM topics tp
		LEFT JOIN track_sources s ON (tp.chat_id = s.chat_id OR s.chat_id = ? OR s.chat_id = ?) AND tp.id = s.topic_id
		LEFT JOIN tracks t ON s.track_id = t.id
		WHERE (tp.chat_id = ? OR tp.chat_id = ?)
		GROUP BY tp.id, tp.chat_id
		UNION
		SELECT s.topic_id, s.chat_id, CAST(s.topic_id AS TEXT), COUNT(DISTINCT s.track_id), 0, 0, 1,
		       COALESCE(NULLIF(MAX(t.cover_url), ''), MAX(t.thumb_url), '')
		FROM track_sources s
		LEFT JOIN tracks t ON s.track_id = t.id
		WHERE (s.chat_id = ? OR s.chat_id = ?) AND s.topic_id > 0 AND s.topic_id NOT IN (SELECT id FROM topics WHERE (chat_id = ? OR chat_id = ?))
		GROUP BY s.topic_id, s.chat_id
		ORDER BY 1 ASC
	`, norm, alt, norm, alt, norm, alt, norm, alt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var topics []*models.Topic
	for rows.Next() {
		var t models.Topic
		var isFolder int
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Name, &t.TrackCount, &t.IconColor, &t.IconEmojiID, &isFolder, &t.ArtworkURL); err != nil {
			return nil, err
		}
		t.IsFolder = isFolder != 0
		topics = append(topics, &t)
	}
	return topics, rows.Err()
}

// CreatePlaylist creates or updates a playlist entity.
func (d *DB) CreatePlaylist(ctx context.Context, pl *models.Playlist) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if pl.CreatedAt.IsZero() {
		pl.CreatedAt = now
	}
	pl.UpdatedAt = now

	isFolderInt := 0
	if pl.IsFolder {
		isFolderInt = 1
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO playlists (id, chat_id, topic_id, title, subtitle, artwork_url, is_folder, parent_folder_id, track_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			subtitle = excluded.subtitle,
			artwork_url = excluded.artwork_url,
			is_folder = excluded.is_folder,
			parent_folder_id = excluded.parent_folder_id,
			track_count = excluded.track_count,
			updated_at = excluded.updated_at;
	`, pl.ID, pl.ChatID, pl.TopicID, pl.Title, pl.Subtitle, pl.ArtworkURL, isFolderInt, pl.ParentFolderID, pl.TrackCount, pl.CreatedAt, pl.UpdatedAt)
	return err
}

// GetPlaylist fetches a playlist by its unique ID.
func (d *DB) GetPlaylist(ctx context.Context, id string) (*models.Playlist, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var pl models.Playlist
	var isFolder int
	err := d.db.QueryRowContext(ctx, `
		SELECT p.id, p.chat_id, p.topic_id, p.title, p.subtitle, p.artwork_url, p.is_folder,
		       p.parent_folder_id, COUNT(pt.track_id), p.created_at, p.updated_at
		FROM playlists p
		LEFT JOIN playlist_tracks pt ON p.id = pt.playlist_id
		WHERE p.id = ?
		GROUP BY p.id
	`, id).Scan(&pl.ID, &pl.ChatID, &pl.TopicID, &pl.Title, &pl.Subtitle, &pl.ArtworkURL,
		&isFolder, &pl.ParentFolderID, &pl.TrackCount, &pl.CreatedAt, &pl.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	pl.IsFolder = isFolder != 0
	return &pl, nil
}

// ListPlaylistsByTopic returns all user playlists belonging to a specific topic folder.
func (d *DB) ListPlaylistsByTopic(ctx context.Context, chatID int64, topicID int32) ([]*models.Playlist, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	norm, alt := NormalizeChatID(chatID)
	rows, err := d.db.QueryContext(ctx, `
		SELECT p.id, p.chat_id, p.topic_id, p.title, p.subtitle, p.artwork_url, p.is_folder,
		       p.parent_folder_id, COUNT(pt.track_id), p.created_at, p.updated_at
		FROM playlists p
		LEFT JOIN playlist_tracks pt ON p.id = pt.playlist_id
		WHERE (p.chat_id = ? OR p.chat_id = ?) AND p.topic_id = ?
		GROUP BY p.id
		ORDER BY p.updated_at DESC
	`, norm, alt, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []*models.Playlist
	for rows.Next() {
		var pl models.Playlist
		var isFolder int
		if err := rows.Scan(&pl.ID, &pl.ChatID, &pl.TopicID, &pl.Title, &pl.Subtitle, &pl.ArtworkURL,
			&isFolder, &pl.ParentFolderID, &pl.TrackCount, &pl.CreatedAt, &pl.UpdatedAt); err != nil {
			return nil, err
		}
		pl.IsFolder = isFolder != 0
		playlists = append(playlists, &pl)
	}
	return playlists, rows.Err()
}

// ListAllPlaylists returns all playlists for a chat.
func (d *DB) ListAllPlaylists(ctx context.Context, chatID int64) ([]*models.Playlist, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	norm, alt := NormalizeChatID(chatID)
	rows, err := d.db.QueryContext(ctx, `
		SELECT p.id, p.chat_id, p.topic_id, p.title, p.subtitle, p.artwork_url, p.is_folder,
		       p.parent_folder_id, COUNT(pt.track_id), p.created_at, p.updated_at
		FROM playlists p
		LEFT JOIN playlist_tracks pt ON p.id = pt.playlist_id
		WHERE (p.chat_id = ? OR p.chat_id = ?)
		GROUP BY p.id
		ORDER BY p.updated_at DESC
	`, norm, alt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []*models.Playlist
	for rows.Next() {
		var pl models.Playlist
		var isFolder int
		if err := rows.Scan(&pl.ID, &pl.ChatID, &pl.TopicID, &pl.Title, &pl.Subtitle, &pl.ArtworkURL,
			&isFolder, &pl.ParentFolderID, &pl.TrackCount, &pl.CreatedAt, &pl.UpdatedAt); err != nil {
			return nil, err
		}
		pl.IsFolder = isFolder != 0
		playlists = append(playlists, &pl)
	}
	return playlists, rows.Err()
}

// AddTrackToPlaylist adds a track to a playlist at a specific position.
func (d *DB) AddTrackToPlaylist(ctx context.Context, playlistID, trackID string, position int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO playlist_tracks (playlist_id, track_id, position, added_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(playlist_id, track_id) DO UPDATE SET
			position = excluded.position;
	`, playlistID, trackID, position, now)
	if err == nil {
		_, _ = d.db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, playlistID)
	}
	return err
}

// RemoveTrackFromPlaylist removes a track from a playlist.
func (d *DB) RemoveTrackFromPlaylist(ctx context.Context, playlistID, trackID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `DELETE FROM playlist_tracks WHERE playlist_id = ? AND track_id = ?`, playlistID, trackID)
	if err == nil {
		_, _ = d.db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now(), playlistID)
	}
	return err
}

// GetPlaylistTracks returns the ordered tracks inside a playlist.
func (d *DB) GetPlaylistTracks(ctx context.Context, playlistID string, offset, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id, t.title, t.artist, t.album, t.duration_sec, t.mime_type, t.file_size, t.file_name, t.created_at,
		       COALESCE(t.fingerprint, ''), COALESCE(t.cover_url, ''), COALESCE(t.thumb_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.year, 0)
		FROM playlist_tracks pt
		JOIN tracks t ON pt.track_id = t.id
		WHERE pt.playlist_id = ?
		ORDER BY pt.position ASC, pt.added_at ASC
		LIMIT ? OFFSET ?
	`, playlistID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var tr models.Track
		if err := rows.Scan(&tr.ID, &tr.Title, &tr.Artist, &tr.Album, &tr.DurationSec, &tr.MimeType,
			&tr.FileSize, &tr.FileName, &tr.CreatedAt, &tr.Fingerprint, &tr.CoverURL, &tr.ThumbURL,
			&tr.Lyrics, &tr.SyncedLyrics, &tr.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &tr)
	}
	return tracks, rows.Err()
}

// SaveBackupRecord saves a record of an uploaded or discovered backup.
func (d *DB) SaveBackupRecord(ctx context.Context, b *models.BackupRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO backups (msg_id, chat_id, topic_id, doc_id, access_hash, file_reference, caption, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(msg_id) DO UPDATE SET
			doc_id = excluded.doc_id,
			access_hash = excluded.access_hash,
			file_reference = excluded.file_reference,
			caption = excluded.caption;
	`, b.MsgID, b.ChatID, b.TopicID, b.DocID, b.AccessHash, b.FileReference, b.Caption, b.CreatedAt)
	return err
}

// GetLatestBackup returns the most recent backup record for a chat and optional topic.
func (d *DB) GetLatestBackup(ctx context.Context, chatID int64, topicID int32) (*models.BackupRecord, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var query string
	var args []interface{}
	if topicID > 1 {
		query = `SELECT msg_id, chat_id, topic_id, doc_id, access_hash, file_reference, caption, created_at
		         FROM backups WHERE chat_id = ? AND topic_id = ? ORDER BY msg_id DESC LIMIT 1`
		args = []interface{}{chatID, topicID}
	} else {
		query = `SELECT msg_id, chat_id, topic_id, doc_id, access_hash, file_reference, caption, created_at
		         FROM backups WHERE chat_id = ? ORDER BY msg_id DESC LIMIT 1`
		args = []interface{}{chatID}
	}

	var b models.BackupRecord
	err := d.db.QueryRowContext(ctx, query, args...).Scan(
		&b.MsgID, &b.ChatID, &b.TopicID, &b.DocID, &b.AccessHash, &b.FileReference, &b.Caption, &b.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// ListTracks returns paginated canonical tracks.
func (d *DB) ListTracks(ctx context.Context, offset, limit int) ([]*models.Track, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var total int
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracks`).Scan(&total)

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks ORDER BY created_at DESC LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, 0, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, total, rows.Err()
}

// ListTracksByTopic returns tracks belonging to a specific topic playlist.
func (d *DB) ListTracksByTopic(ctx context.Context, chatID int64, topicID int32, offset, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	norm, alt := NormalizeChatID(chatID)
	rows, err := d.db.QueryContext(ctx, `
		SELECT DISTINCT t.id, t.title, t.artist, t.album, t.duration_sec, t.mime_type, t.file_size, t.file_name, t.created_at,
		       COALESCE(t.fingerprint, ''), COALESCE(t.cover_url, ''), COALESCE(t.thumb_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.year, 0)
		FROM track_sources s
		JOIN tracks t ON s.track_id = t.id
		WHERE (s.chat_id = ? OR s.chat_id = ?) AND s.topic_id = ?
		ORDER BY s.msg_id ASC
		LIMIT ? OFFSET ?
	`, norm, alt, topicID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// ListArtists returns aggregated distinct artists with track counts and artwork.
func (d *DB) ListArtists(ctx context.Context, offset, limit int) ([]*models.Artist, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var total int
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT artist) FROM tracks WHERE artist != ''`).Scan(&total)

	rows, err := d.db.QueryContext(ctx, `
		SELECT artist, COUNT(*) as track_count, COALESCE(NULLIF(MAX(thumb_url), ''), MAX(cover_url), '') as thumb
		FROM tracks
		WHERE artist != ''
		GROUP BY artist
		ORDER BY track_count DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var artists []*models.Artist
	for rows.Next() {
		var a models.Artist
		if err := rows.Scan(&a.Name, &a.TrackCount, &a.ThumbURL); err != nil {
			return nil, 0, err
		}
		a.ID = fmt.Sprintf("artist_%s", a.Name)
		artists = append(artists, &a)
	}
	return artists, total, rows.Err()
}

// ListAlbums returns aggregated distinct albums with artist, year, track count, and coverart.
func (d *DB) ListAlbums(ctx context.Context, offset, limit int) ([]*models.Album, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var total int
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT album) FROM tracks WHERE album != ''`).Scan(&total)

	rows, err := d.db.QueryContext(ctx, `
		SELECT album, MAX(artist), MAX(year), COUNT(*) as track_count, COALESCE(NULLIF(MAX(cover_url), ''), MAX(thumb_url), '')
		FROM tracks
		WHERE album != ''
		GROUP BY album
		ORDER BY album ASC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var albums []*models.Album
	for rows.Next() {
		var a models.Album
		if err := rows.Scan(&a.Title, &a.Artist, &a.Year, &a.TrackCount, &a.CoverURL); err != nil {
			return nil, 0, err
		}
		a.ID = fmt.Sprintf("album_%s", a.Title)
		albums = append(albums, &a)
	}
	return albums, total, rows.Err()
}

// GetArtistTracks returns tracks by artist.
func (d *DB) GetArtistTracks(ctx context.Context, artist string, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		WHERE LOWER(artist) = LOWER(?)
		ORDER BY title ASC LIMIT ?
	`, artist, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// GetAlbumTracks returns tracks by album.
func (d *DB) GetAlbumTracks(ctx context.Context, album string, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		WHERE LOWER(album) = LOWER(?)
		ORDER BY id ASC LIMIT ?
	`, album, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// SearchTracks performs case-insensitive search across title, artist, and album.
func (d *DB) SearchTracks(ctx context.Context, query string, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	param := "%" + strings.ToLower(query) + "%"
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		WHERE LOWER(title) LIKE ? OR LOWER(artist) LIKE ? OR LOWER(album) LIKE ?
		ORDER BY title ASC LIMIT ?
	`, param, param, param, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// GetRandomTracks returns randomized tracks for Daily Mixes / automated playlists.
func (d *DB) GetRandomTracks(ctx context.Context, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(cover_url, ''), COALESCE(thumb_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		ORDER BY RANDOM() LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.CoverURL, &t.ThumbURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// SaveWorkerLocation stores or refreshes bot-specific access hash & file reference.
func (d *DB) SaveWorkerLocation(ctx context.Context, loc *models.WorkerLocation) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO worker_file_refs (bot_id, msg_id, doc_id, access_hash, file_reference, dc_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bot_id, msg_id) DO UPDATE SET
			doc_id = excluded.doc_id,
			access_hash = excluded.access_hash,
			file_reference = excluded.file_reference,
			dc_id = excluded.dc_id,
			updated_at = excluded.updated_at;
	`, loc.BotID, loc.MsgID, loc.DocID, loc.AccessHash, loc.FileReference, loc.DCID, time.Now())
	return err
}

// GetWorkerLocation retrieves a worker's cached location for a message ID.
func (d *DB) GetWorkerLocation(ctx context.Context, botID string, msgID int32) (*models.WorkerLocation, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.db.QueryRowContext(ctx, `
		SELECT bot_id, msg_id, doc_id, access_hash, file_reference, dc_id, updated_at
		FROM worker_file_refs WHERE bot_id = ? AND msg_id = ?
	`, botID, msgID)

	var loc models.WorkerLocation
	err := row.Scan(&loc.BotID, &loc.MsgID, &loc.DocID, &loc.AccessHash, &loc.FileReference, &loc.DCID, &loc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &loc, nil
}

// SaveSyncState updates the persisted gotd pts / max_msg_id for a channel.
func (d *DB) SaveSyncState(ctx context.Context, state *models.SyncState) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO sync_state (channel_id, pts, date, qts, max_msg_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			pts = MAX(sync_state.pts, excluded.pts),
			date = MAX(sync_state.date, excluded.date),
			qts = MAX(sync_state.qts, excluded.qts),
			max_msg_id = MAX(sync_state.max_msg_id, excluded.max_msg_id),
			updated_at = excluded.updated_at;
	`, state.ChannelID, state.Pts, state.Date, state.Qts, state.MaxMsgID, time.Now())
	return err
}

// GetSyncState retrieves the last saved sync state for a channel.
func (d *DB) GetSyncState(ctx context.Context, channelID int64) (*models.SyncState, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.db.QueryRowContext(ctx, `
		SELECT channel_id, pts, date, qts, max_msg_id, updated_at
		FROM sync_state WHERE channel_id = ?
	`, channelID)

	var s models.SyncState
	err := row.Scan(&s.ChannelID, &s.Pts, &s.Date, &s.Qts, &s.MaxMsgID, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &models.SyncState{ChannelID: channelID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ExportBackup creates a clean, vacuumed snapshot of library.db for Telegram backup.
// Secrets are NOT stored in SQLite, so the database file is safe to back up.
func (d *DB) ExportBackup(destPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
		return err
	}
	_ = os.Remove(destPath)

	// Execute VACUUM INTO to produce a consistent single-file SQLite database
	_, err := d.db.Exec(fmt.Sprintf("VACUUM INTO '%s';", destPath))
	return err
}

// ImportBackup replaces the local database with a restored snapshot file.
// Last-write-wins: the restored file overwrites local library state and runs schema migrations.
func (d *DB) ImportBackup(srcPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Verify source exists and is non-empty
	info, err := os.Stat(srcPath)
	if err != nil || info.Size() < 512 {
		return fmt.Errorf("invalid backup file: %w", err)
	}

	// Close current DB connection before file replacement
	if d.db != nil {
		_ = d.db.Close()
	}

	// Copy backup into active path
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(d.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Remove old WAL and SHM files
	_ = os.Remove(d.path + "-wal")
	_ = os.Remove(d.path + "-shm")

	// Reopen and run migrations on restored database
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", d.path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to reopen restored database: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	d.db = sqlDB

	return d.migrate()
}
