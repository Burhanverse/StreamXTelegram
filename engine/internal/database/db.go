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

	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
)

const CurrentSchemaVersion = 7

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

	const currentSchemaVersion = 8
	if version != currentSchemaVersion {
		// Rule 3: NO MIGRATION CODE. Recreate database from scratch cleanly.
		tables := []string{
			"playlist_tracks",
			"playlists",
			"track_sources",
			"worker_file_refs",
			"enrichment_state",
			"backups",
			"topics",
			"sync_state",
			"tracks",
		}
		for _, tbl := range tables {
			_, _ = d.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s;", tbl))
		}
		_, _ = d.db.ExecContext(ctx, "DELETE FROM schema_migrations;")

		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		schemaSQL := `
			CREATE TABLE sync_state (
				channel_id INTEGER PRIMARY KEY,
				pts INTEGER NOT NULL DEFAULT 0,
				date INTEGER NOT NULL DEFAULT 0,
				qts INTEGER NOT NULL DEFAULT 0,
				max_msg_id INTEGER NOT NULL DEFAULT 0,
				updated_at DATETIME NOT NULL
			);

			CREATE TABLE tracks (
				id TEXT PRIMARY KEY,
				title TEXT NOT NULL,
				artist TEXT NOT NULL,
				artists TEXT NOT NULL DEFAULT '',
				album TEXT NOT NULL,
				album_artist TEXT NOT NULL DEFAULT '',
				track_number INTEGER NOT NULL DEFAULT 0,
				disc_number INTEGER NOT NULL DEFAULT 0,
				year INTEGER NOT NULL DEFAULT 0,
				genre TEXT NOT NULL DEFAULT '',
				composer TEXT NOT NULL DEFAULT '',
				duration_sec INTEGER NOT NULL,
				sample_rate INTEGER NOT NULL DEFAULT 0,
				mime_type TEXT NOT NULL,
				file_size INTEGER NOT NULL,
				file_name TEXT NOT NULL,
				fingerprint TEXT DEFAULT '',
				thumbnail_url TEXT DEFAULT '',
				cover_url TEXT DEFAULT '',
				lyrics TEXT DEFAULT '',
				synced_lyrics TEXT DEFAULT '',
				match_source TEXT DEFAULT '',
				match_score REAL DEFAULT 0.0,
				matched_at DATETIME,
				meta_version INTEGER DEFAULT 3,
				created_at DATETIME NOT NULL
			);
			CREATE INDEX idx_tracks_artist ON tracks(artist);
			CREATE INDEX idx_tracks_album ON tracks(album);
			CREATE INDEX idx_tracks_album_group ON tracks(album, album_artist, year);

			CREATE TABLE track_sources (
				track_id TEXT NOT NULL,
				chat_id INTEGER NOT NULL,
				msg_id INTEGER NOT NULL,
				topic_id INTEGER NOT NULL DEFAULT 0,
				file_unique_id TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY(track_id, chat_id, msg_id),
				FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);
			CREATE INDEX idx_sources_msg ON track_sources(chat_id, msg_id);
			CREATE INDEX idx_sources_topic ON track_sources(topic_id);
			CREATE INDEX idx_sources_file_unique ON track_sources(file_unique_id);

			CREATE TABLE worker_file_refs (
				bot_id TEXT NOT NULL,
				msg_id INTEGER NOT NULL,
				doc_id INTEGER NOT NULL DEFAULT 0,
				access_hash INTEGER NOT NULL,
				file_reference BLOB NOT NULL,
				dc_id INTEGER NOT NULL DEFAULT 2,
				updated_at DATETIME NOT NULL,
				PRIMARY KEY(bot_id, msg_id)
			);

			CREATE TABLE topics (
				id INTEGER NOT NULL,
				chat_id INTEGER NOT NULL,
				name TEXT NOT NULL,
				track_count INTEGER NOT NULL DEFAULT 0,
				icon_color INTEGER DEFAULT 0,
				icon_emoji_id INTEGER DEFAULT 0,
				is_folder BOOLEAN DEFAULT 1,
				artwork_url TEXT DEFAULT '',
				updated_at DATETIME NOT NULL,
				PRIMARY KEY(id, chat_id)
			);

			CREATE TABLE playlists (
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
			);
			CREATE INDEX idx_playlists_topic ON playlists(topic_id);
			CREATE INDEX idx_playlists_parent ON playlists(parent_folder_id);

			CREATE TABLE playlist_tracks (
				playlist_id TEXT NOT NULL,
				track_id TEXT NOT NULL,
				position INTEGER NOT NULL,
				added_at DATETIME NOT NULL,
				PRIMARY KEY (playlist_id, track_id),
				FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
				FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);
			CREATE INDEX idx_pl_tracks_pos ON playlist_tracks(playlist_id, position);

			CREATE TABLE backups (
				msg_id INTEGER PRIMARY KEY,
				chat_id INTEGER NOT NULL,
				topic_id INTEGER NOT NULL,
				doc_id INTEGER NOT NULL DEFAULT 0,
				access_hash INTEGER NOT NULL DEFAULT 0,
				file_reference BLOB,
				caption TEXT DEFAULT '',
				created_at DATETIME NOT NULL
			);
			CREATE INDEX idx_backups_topic ON backups(chat_id, topic_id);

			CREATE TABLE enrichment_state (
				track_id TEXT PRIMARY KEY,
				status TEXT NOT NULL,
				attempts INTEGER NOT NULL DEFAULT 0,
				last_attempt DATETIME NOT NULL,
				reason TEXT NOT NULL DEFAULT '',
				expires_at DATETIME NOT NULL,
				FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
			);
			CREATE INDEX idx_enrichment_state_status ON enrichment_state(status, expires_at);
		`

		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("failed creating schema v7: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?);`, currentSchemaVersion, time.Now()); err != nil {
			return err
		}

		return tx.Commit()
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

	artistsStr := ""
	if len(t.Artists) > 0 {
		artistsStr = strings.Join(t.Artists, ", ")
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO tracks (
			id, title, artist, artists, album, album_artist, track_number, disc_number,
			year, genre, composer, duration_sec, sample_rate, mime_type, file_size, file_name,
			fingerprint, thumbnail_url, cover_url, lyrics, synced_lyrics, match_source, match_score,
			matched_at, meta_version, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = CASE WHEN tracks.match_source = 'embedded' AND tracks.title != '' THEN tracks.title ELSE excluded.title END,
			artist = CASE WHEN tracks.match_source = 'embedded' AND tracks.artist != '' THEN tracks.artist ELSE excluded.artist END,
			album = CASE WHEN tracks.match_source = 'embedded' AND tracks.album != '' THEN tracks.album ELSE excluded.album END,
			duration_sec = CASE WHEN tracks.match_source = 'embedded' AND tracks.duration_sec > 0 THEN tracks.duration_sec ELSE excluded.duration_sec END,
			mime_type = excluded.mime_type,
			file_size = excluded.file_size,
			file_name = excluded.file_name,
			fingerprint = CASE WHEN excluded.fingerprint != '' THEN excluded.fingerprint ELSE tracks.fingerprint END,
			thumbnail_url = CASE WHEN excluded.thumbnail_url != '' THEN excluded.thumbnail_url ELSE tracks.thumbnail_url END,
			cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
			lyrics = CASE WHEN excluded.lyrics != '' THEN excluded.lyrics ELSE tracks.lyrics END,
			synced_lyrics = CASE WHEN excluded.synced_lyrics != '' THEN excluded.synced_lyrics ELSE tracks.synced_lyrics END,
			year = CASE WHEN tracks.year > 0 THEN tracks.year ELSE excluded.year END;
	`,
		t.ID, t.Title, t.Artist, artistsStr, t.Album, t.AlbumArtist, t.TrackNumber, t.DiscNumber,
		t.Year, t.Genre, t.Composer, t.DurationSec, t.SampleRate, t.MimeType, t.FileSize, t.FileName,
		t.Fingerprint, t.ThumbnailURL, t.CoverURL, t.Lyrics, t.SyncedLyrics, t.MatchSource, t.MatchScore,
		t.MatchedAt, t.MetaVersion, t.CreatedAt,
	)
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
		INSERT INTO tracks (
			id, title, artist, artists, album, album_artist, track_number, disc_number,
			year, genre, composer, duration_sec, sample_rate, mime_type, file_size, file_name,
			fingerprint, thumbnail_url, cover_url, lyrics, synced_lyrics, match_source, match_score,
			matched_at, meta_version, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = CASE WHEN tracks.match_source = 'embedded' AND tracks.title != '' THEN tracks.title ELSE excluded.title END,
			artist = CASE WHEN tracks.match_source = 'embedded' AND tracks.artist != '' THEN tracks.artist ELSE excluded.artist END,
			album = CASE WHEN tracks.match_source = 'embedded' AND tracks.album != '' THEN tracks.album ELSE excluded.album END,
			duration_sec = CASE WHEN tracks.match_source = 'embedded' AND tracks.duration_sec > 0 THEN tracks.duration_sec ELSE excluded.duration_sec END,
			mime_type = excluded.mime_type,
			file_size = excluded.file_size,
			file_name = excluded.file_name,
			fingerprint = CASE WHEN excluded.fingerprint != '' THEN excluded.fingerprint ELSE tracks.fingerprint END,
			thumbnail_url = CASE WHEN excluded.thumbnail_url != '' THEN excluded.thumbnail_url ELSE tracks.thumbnail_url END,
			cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
			lyrics = CASE WHEN excluded.lyrics != '' THEN excluded.lyrics ELSE tracks.lyrics END,
			synced_lyrics = CASE WHEN excluded.synced_lyrics != '' THEN excluded.synced_lyrics ELSE tracks.synced_lyrics END,
			year = CASE WHEN tracks.year > 0 THEN tracks.year ELSE excluded.year END;
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
		artistsStr := ""
		if len(t.Artists) > 0 {
			artistsStr = strings.Join(t.Artists, ", ")
		}
		_, err := stmt.ExecContext(ctx,
			t.ID, t.Title, t.Artist, artistsStr, t.Album, t.AlbumArtist, t.TrackNumber, t.DiscNumber,
			t.Year, t.Genre, t.Composer, t.DurationSec, t.SampleRate, t.MimeType, t.FileSize, t.FileName,
			t.Fingerprint, t.ThumbnailURL, t.CoverURL, t.Lyrics, t.SyncedLyrics, t.MatchSource, t.MatchScore,
			t.MatchedAt, t.MetaVersion, createdAt,
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
		SELECT t.id, t.title, t.artist, COALESCE(t.artists, ''), t.album, COALESCE(t.album_artist, ''),
		       COALESCE(t.track_number, 0), COALESCE(t.disc_number, 0), COALESCE(t.year, 0),
		       COALESCE(t.genre, ''), COALESCE(t.composer, ''), t.duration_sec, COALESCE(t.sample_rate, 0),
		       t.mime_type, t.file_size, t.file_name, t.created_at,
		       COALESCE(t.fingerprint, ''), COALESCE(t.thumbnail_url, ''), COALESCE(t.cover_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.match_source, ''),
		       COALESCE(t.match_score, 0.0), COALESCE(t.meta_version, 3),
		       COALESCE(s.chat_id, 0), COALESCE(s.msg_id, 0)
		FROM tracks t
		LEFT JOIN track_sources s ON t.id = s.track_id
		WHERE t.id = ?
		LIMIT 1
	`, id)

	var t models.Track
	var artistsStr string
	err := row.Scan(
		&t.ID, &t.Title, &t.Artist, &artistsStr, &t.Album, &t.AlbumArtist,
		&t.TrackNumber, &t.DiscNumber, &t.Year,
		&t.Genre, &t.Composer, &t.DurationSec, &t.SampleRate,
		&t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL,
		&t.Lyrics, &t.SyncedLyrics, &t.MatchSource,
		&t.MatchScore, &t.MetaVersion,
		&t.ChatID, &t.MsgID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if artistsStr != "" {
		for _, a := range strings.Split(artistsStr, ",") {
			if s := strings.TrimSpace(a); s != "" {
				t.Artists = append(t.Artists, s)
			}
		}
	}
	return &t, nil
}

// UpdateTrackEnrichment updates external metadata fields on an existing track record.
func (d *DB) UpdateTrackEnrichment(ctx context.Context, id, coverURL, thumbnailURL, lyrics, syncedLyrics string, year int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
			thumbnail_url = CASE WHEN ? != '' THEN ? ELSE thumbnail_url END,
			lyrics = CASE WHEN ? != '' THEN ? ELSE lyrics END,
			synced_lyrics = CASE WHEN ? != '' THEN ? ELSE synced_lyrics END,
			year = CASE WHEN ? > 0 THEN ? ELSE year END
		WHERE id = ?;
	`, coverURL, coverURL, thumbnailURL, thumbnailURL, lyrics, lyrics, syncedLyrics, syncedLyrics, year, year, id)
	return err
}

// UpdateTrackEnrichmentWithAlbum updates external metadata fields including album on an existing track record.
func (d *DB) UpdateTrackEnrichmentWithAlbum(ctx context.Context, id, album, coverURL, thumbnailURL, lyrics, syncedLyrics string, year int) error {
	return d.UpdateTrackFullMetadata(ctx, id, "", "", album, coverURL, thumbnailURL, lyrics, syncedLyrics, year)
}

// UpdateTrackFullMetadata updates title, artist, album, cover_url, thumbnail_url, lyrics, synced_lyrics, and year.
// If track was already enriched from embedded authoritative tags (match_source = 'embedded'),
// it never overwrites the embedded title, artist, album, or year.
func (d *DB) UpdateTrackFullMetadata(ctx context.Context, id, title, artist, album, coverURL, thumbnailURL, lyrics, syncedLyrics string, year int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			title = CASE WHEN (match_source != 'embedded' OR match_source IS NULL) AND ? != '' THEN ? ELSE title END,
			artist = CASE WHEN (match_source != 'embedded' OR match_source IS NULL) AND ? != '' AND ? != 'Unknown Artist' THEN ? ELSE artist END,
			album = CASE WHEN (match_source != 'embedded' OR match_source IS NULL) AND ? != '' AND ? != 'Telegram Music' THEN ? ELSE album END,
			cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
			thumbnail_url = CASE WHEN ? != '' THEN ? ELSE thumbnail_url END,
			lyrics = CASE WHEN ? != '' THEN ? ELSE lyrics END,
			synced_lyrics = CASE WHEN ? != '' THEN ? ELSE synced_lyrics END,
			year = CASE WHEN (match_source != 'embedded' OR match_source IS NULL) AND ? > 0 THEN ? ELSE year END
		WHERE id = ?;
	`, title, title, artist, artist, artist, album, album, album, coverURL, coverURL, thumbnailURL, thumbnailURL, lyrics, lyrics, syncedLyrics, syncedLyrics, year, year, id)
	return err
}

// SaveEmbeddedTrackMetadata writes authoritative embedded metadata to the tracks table with match_source = 'embedded'.
func (d *DB) SaveEmbeddedTrackMetadata(ctx context.Context, trackID string, meta *metadata.FLACMetadata, coverURL, thumbnailURL string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	artistsStr := ""
	if len(meta.Artists) > 0 {
		artistsStr = strings.Join(meta.Artists, ", ")
	}

	_, err := d.db.ExecContext(ctx, `
		UPDATE tracks SET
			title = CASE WHEN ? != '' THEN ? ELSE title END,
			artist = CASE WHEN ? != '' THEN ? ELSE artist END,
			artists = CASE WHEN ? != '' THEN ? ELSE artists END,
			album = CASE WHEN ? != '' THEN ? ELSE album END,
			album_artist = CASE WHEN ? != '' THEN ? ELSE album_artist END,
			year = CASE WHEN ? > 0 THEN ? ELSE year END,
			track_number = CASE WHEN ? > 0 THEN ? ELSE track_number END,
			disc_number = CASE WHEN ? > 0 THEN ? ELSE disc_number END,
			genre = CASE WHEN ? != '' THEN ? ELSE genre END,
			composer = CASE WHEN ? != '' THEN ? ELSE composer END,
			duration_sec = CASE WHEN ? > 0 THEN ? ELSE duration_sec END,
			sample_rate = CASE WHEN ? > 0 THEN ? ELSE sample_rate END,
			cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
			thumbnail_url = CASE WHEN ? != '' THEN ? ELSE thumbnail_url END,
			match_source = 'embedded',
			match_score = 1.0,
			matched_at = ?,
			meta_version = 3
		WHERE id = ?;
	`,
		meta.Title, meta.Title,
		meta.Artist, meta.Artist,
		artistsStr, artistsStr,
		meta.Album, meta.Album,
		meta.AlbumArtist, meta.AlbumArtist,
		meta.Year, meta.Year,
		meta.TrackNumber, meta.TrackNumber,
		meta.DiscNumber, meta.DiscNumber,
		meta.Genre, meta.Genre,
		meta.Composer, meta.Composer,
		meta.DurationSec, meta.DurationSec,
		meta.SampleRate, meta.SampleRate,
		coverURL, coverURL,
		thumbnailURL, thumbnailURL,
		time.Now(),
		trackID,
	)
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

// GetEnrichmentAttempts returns the number of times enrichment has been attempted for trackID.
func (d *DB) GetEnrichmentAttempts(ctx context.Context, trackID string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var attempts int
	err := d.db.QueryRowContext(ctx, `SELECT attempts FROM enrichment_state WHERE track_id = ?;`, trackID).Scan(&attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return attempts, nil
}

// GetEnrichmentStatus returns the current enrichment status for trackID, or "" if not found.
func (d *DB) GetEnrichmentStatus(ctx context.Context, trackID string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var status string
	err := d.db.QueryRowContext(ctx, `SELECT status FROM enrichment_state WHERE track_id = ?;`, trackID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return status, nil
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
			attempts = CASE WHEN excluded.attempts > 0 THEN excluded.attempts ELSE attempts + 1 END,
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
		WHERE track_id = ? AND expires_at > ?;
	`, trackID, time.Now()).Scan(&count)
	return err == nil && count > 0
}

// ListUnenrichedTrackIDs returns track IDs that need embedded extraction or online cover art fallback,
// respecting negative cache TTL and capped at batch limit.
func (d *DB) ListUnenrichedTrackIDs(ctx context.Context, batchLimit ...int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	limit := 20
	if len(batchLimit) > 0 && batchLimit[0] > 0 {
		limit = batchLimit[0]
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id FROM tracks t
		LEFT JOIN enrichment_state e ON t.id = e.track_id
		WHERE (e.track_id IS NULL OR e.status != 'success')
		  AND (e.track_id IS NULL OR e.expires_at <= ?)
		  AND (
			(t.match_source IS NULL OR t.match_source = '')
			OR (t.thumbnail_url IS NULL OR t.thumbnail_url = '')
		  )
		ORDER BY t.created_at DESC
		LIMIT ?;
	`, time.Now(), limit)
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

// EnrichmentDiagnostics represents the summary counts of library enrichment state.
type EnrichmentDiagnostics struct {
	TotalTracks    int
	EmbeddedOK     int
	NoArt          int
	TransientFails int
	PermanentFails int
	OnlineMatched  int
	StillPending   int
}

// GetEnrichmentDiagnostics computes exact counts across tracks and enrichment_state.
func (d *DB) GetEnrichmentDiagnostics(ctx context.Context) (EnrichmentDiagnostics, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var diag EnrichmentDiagnostics
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tracks`).Scan(&diag.TotalTracks)
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tracks WHERE match_source = 'embedded' AND thumbnail_url != ''`).Scan(&diag.EmbeddedOK)
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tracks WHERE (thumbnail_url IS NULL OR thumbnail_url = '') AND (cover_url IS NULL OR cover_url = '')`).Scan(&diag.NoArt)
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM enrichment_state WHERE status = 'transient_retry'`).Scan(&diag.TransientFails)
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM enrichment_state WHERE status = 'failed'`).Scan(&diag.PermanentFails)
	_ = d.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tracks WHERE match_source != '' AND match_source != 'embedded'`).Scan(&diag.OnlineMatched)
	_ = d.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM tracks t
		LEFT JOIN enrichment_state e ON t.id = e.track_id
		WHERE (t.match_source IS NULL OR t.match_source = '')
		   OR ((t.thumbnail_url IS NULL OR t.thumbnail_url = '') AND (e.track_id IS NULL OR e.status != 'failed'))
	`).Scan(&diag.StillPending)

	return diag, nil
}

// GetEnrichmentSummaryLine returns the formatted summary line requested by Item 11:
// [ENRICH_SUMMARY] total=%d embedded_ok=%d no_art=%d transient_fails=%d permanent_fails=%d online_matched=%d pending=%d
func (d *DB) GetEnrichmentSummaryLine(ctx context.Context) string {
	diag, err := d.GetEnrichmentDiagnostics(ctx)
	if err != nil {
		return fmt.Sprintf("[ENRICH_SUMMARY] error: %v", err)
	}
	return fmt.Sprintf("[ENRICH_SUMMARY] total=%d embedded_ok=%d no_art=%d transient_fails=%d permanent_fails=%d online_matched=%d pending=%d",
		diag.TotalTracks, diag.EmbeddedOK, diag.NoArt, diag.TransientFails, diag.PermanentFails, diag.OnlineMatched, diag.StillPending)
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks WHERE fingerprint = ? LIMIT 1
	`, fingerprint)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
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
		&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
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
		       COALESCE(t.fingerprint, ''), COALESCE(t.thumbnail_url, ''), COALESCE(t.cover_url, ''),
		       COALESCE(t.lyrics, ''), COALESCE(t.synced_lyrics, ''), COALESCE(t.year, 0)
		FROM track_sources s
		JOIN tracks t ON s.track_id = t.id
		WHERE s.file_unique_id = ? LIMIT 1
	`, fileUniqueID)

	var t models.Track
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
		&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year)
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
		       COALESCE(NULLIF(MAX(t.thumbnail_url), ''), MAX(t.cover_url), ''),
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
				       COALESCE(NULLIF(MAX(t.thumbnail_url), ''), MAX(t.cover_url), '')
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
		       COALESCE(NULLIF(MAX(t.thumbnail_url), ''), MAX(t.cover_url), '')
		FROM topics tp
		LEFT JOIN track_sources s ON (tp.chat_id = s.chat_id OR s.chat_id = ? OR s.chat_id = ?) AND tp.id = s.topic_id
		LEFT JOIN tracks t ON s.track_id = t.id
		WHERE (tp.chat_id = ? OR tp.chat_id = ?)
		GROUP BY tp.id, tp.chat_id
		UNION
		SELECT s.topic_id, s.chat_id, CAST(s.topic_id AS TEXT), COUNT(DISTINCT s.track_id), 0, 0, 1,
		       COALESCE(NULLIF(MAX(t.thumbnail_url), ''), MAX(t.cover_url), '')
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
		       COALESCE(t.fingerprint, ''), COALESCE(t.thumbnail_url, ''), COALESCE(t.cover_url, ''),
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
			&tr.FileSize, &tr.FileName, &tr.CreatedAt, &tr.Fingerprint, &tr.ThumbnailURL, &tr.CoverURL,
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
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
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
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
		       COALESCE(t.fingerprint, ''), COALESCE(t.thumbnail_url, ''), COALESCE(t.cover_url, ''),
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
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
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
		SELECT artist, COUNT(*) as track_count, COALESCE(NULLIF(MAX(thumbnail_url), ''), MAX(cover_url), '') as thumb
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
		if err := rows.Scan(&a.Name, &a.TrackCount, &a.ThumbnailURL); err != nil {
			return nil, 0, err
		}
		a.ID = fmt.Sprintf("artist_%s", a.Name)
		artists = append(artists, &a)
	}
	return artists, total, rows.Err()
}

// ListAlbums returns aggregated distinct albums grouped by (album, albumartist, year).
func (d *DB) ListAlbums(ctx context.Context, offset, limit int) ([]*models.Album, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var total int
	_ = d.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM (
			SELECT 1 FROM tracks
			WHERE album != ''
			GROUP BY album, COALESCE(NULLIF(album_artist, ''), artist), year
		)
	`).Scan(&total)

	rows, err := d.db.QueryContext(ctx, `
		SELECT album, COALESCE(NULLIF(MAX(album_artist), ''), MAX(artist)), year, COUNT(*) as track_count, COALESCE(NULLIF(MAX(thumbnail_url), ''), MAX(cover_url), '')
		FROM tracks
		WHERE album != ''
		GROUP BY album, COALESCE(NULLIF(album_artist, ''), artist), year
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
		if err := rows.Scan(&a.Title, &a.Artist, &a.Year, &a.TrackCount, &a.ThumbnailURL); err != nil {
			return nil, 0, err
		}
		a.CoverURL = a.ThumbnailURL
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
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
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
			return nil, err
		}
		tracks = append(tracks, &t)
	}
	return tracks, rows.Err()
}

// GetAlbumTracks returns tracks by album ordered by disc and track number.
func (d *DB) GetAlbumTracks(ctx context.Context, album string, limit int) ([]*models.Track, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_sec, mime_type, file_size, file_name, created_at,
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
		       COALESCE(lyrics, ''), COALESCE(synced_lyrics, ''), COALESCE(year, 0)
		FROM tracks
		WHERE LOWER(album) = LOWER(?)
		ORDER BY disc_number ASC, track_number ASC, title ASC LIMIT ?
	`, album, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tracks []*models.Track
	for rows.Next() {
		var t models.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.DurationSec, &t.MimeType, &t.FileSize, &t.FileName, &t.CreatedAt,
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
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
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
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
		       COALESCE(fingerprint, ''), COALESCE(thumbnail_url, ''), COALESCE(cover_url, ''),
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
			&t.Fingerprint, &t.ThumbnailURL, &t.CoverURL, &t.Lyrics, &t.SyncedLyrics, &t.Year); err != nil {
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
