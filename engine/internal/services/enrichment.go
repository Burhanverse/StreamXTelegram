package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/logger"
	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
)

var logEnrich = logger.New("enrichment")

// LyricsResult holds lyrics retrieved from external providers.
type LyricsResult struct {
	PlainLyrics  string `json:"plain_lyrics"`
	SyncedLyrics string `json:"synced_lyrics"`
	Source       string `json:"source"`
}

// ChunkReader reads audio byte slices for embedded metadata and artwork extraction.
type ChunkReader interface {
	ReadChunk(ctx context.Context, trackID string, offset int64, limit int) ([]byte, error)
}

// HeaderReader reads minimal file headers without prefetching or polluting the playback cache.
type HeaderReader interface {
	ReadHeader(ctx context.Context, trackID string, maxBytes int) ([]byte, error)
}

type providerLimiter struct {
	mu           sync.Mutex
	tokens       float64
	capacity     float64
	refillRate   float64 // tokens per second
	lastRefill   time.Time
	backoffUntil time.Time
}

func newProviderLimiter(rate float64, burst float64) *providerLimiter {
	return &providerLimiter{
		tokens:     burst,
		capacity:   burst,
		refillRate: rate,
		lastRefill: time.Now(),
	}
}

func (l *providerLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Before(l.backoffUntil) {
		return false
	}

	elapsed := now.Sub(l.lastRefill).Seconds()
	l.tokens += elapsed * l.refillRate
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
	l.lastRefill = now

	if l.tokens >= 1.0 {
		l.tokens -= 1.0
		return true
	}
	return false
}

func (l *providerLimiter) Mark429(backoff time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if backoff <= 0 {
		backoff = 30 * time.Second
	}
	l.backoffUntil = time.Now().Add(backoff)
}

// EnrichmentService manages asynchronous metadata enrichment (artwork, lyrics, release year).
type EnrichmentService struct {
	db               *database.DB
	reader           ChunkReader
	artworkDir       string
	httpClient       *http.Client
	queue            chan string
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	mu               sync.RWMutex

	isPlaybackActive func() bool
	isSyncing        func() bool
	lastPlaybackSeen time.Time

	enqueuedMu  sync.Mutex
	enqueuedMap map[string]time.Time

	jsLimiter *providerLimiter
	itLimiter *providerLimiter
	dzLimiter *providerLimiter
	mbLimiter *providerLimiter
}

// NewEnrichmentService creates a new EnrichmentService with single-threaded throttled background worker.
func NewEnrichmentService(db *database.DB) *EnrichmentService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &EnrichmentService{
		db: db,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		queue:       make(chan string, 200),
		ctx:         ctx,
		cancel:      cancel,
		enqueuedMap: make(map[string]time.Time),
		jsLimiter:   newProviderLimiter(2.0, 4.0),
		itLimiter:   newProviderLimiter(1.5, 3.0),
		dzLimiter:   newProviderLimiter(2.0, 4.0),
		mbLimiter:   newProviderLimiter(1.0, 2.0),
	}

	// Throttle: at most 1 concurrent background worker so playback is never starved
	s.wg.Add(1)
	go s.workerLoop()

	return s
}

// SetPlaybackChecker registers a hook to test if playback streaming is currently active.
func (s *EnrichmentService) SetPlaybackChecker(fn func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isPlaybackActive = fn
}

// SetSyncChecker registers a hook to test if initial channel catchup sync is actively running.
func (s *EnrichmentService) SetSyncChecker(fn func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isSyncing = fn
}

// SetChunkReader sets the chunk reader for embedded tag and artwork extraction.
func (s *EnrichmentService) SetChunkReader(reader ChunkReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reader = reader
}

// SetArtworkDir sets the local directory for caching artwork images on disk.
func (s *EnrichmentService) SetArtworkDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artworkDir = dir
	if dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
}

// ArtworkDir returns the configured local artwork cache directory.
func (s *EnrichmentService) ArtworkDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.artworkDir
}

// Stop gracefully stops the background enrichment queue.
func (s *EnrichmentService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	transientKeywords := []string{
		"timeout",
		"deadline exceeded",
		"bot not ready",
		"flood_wait",
		"flood wait",
		"connection reset",
		"broken pipe",
		"network is unreachable",
		"no route to host",
		"temporary failure",
		"eof",
		"too many requests",
		"rate limit",
	}
	for _, kw := range transientKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// Enqueue adds a track ID to the background enrichment queue, deduplicating and skipping negative cached entries.
// Uses bounded queue with backpressure instead of dropping.
func (s *EnrichmentService) Enqueue(trackID string) {
	if trackID == "" {
		return
	}

	// Deduplicate repeated Enqueue requests within 10 minutes
	s.enqueuedMu.Lock()
	if last, exists := s.enqueuedMap[trackID]; exists {
		if time.Since(last) < 10*time.Minute {
			s.enqueuedMu.Unlock()
			return
		}
	}
	if len(s.enqueuedMap) > 5000 {
		now := time.Now()
		for k, t := range s.enqueuedMap {
			if now.Sub(t) > 10*time.Minute {
				delete(s.enqueuedMap, k)
			}
		}
	}
	s.enqueuedMap[trackID] = time.Now()
	s.enqueuedMu.Unlock()

	// Respect negative cache with TTL
	if s.db != nil && s.db.IsEnrichmentNegativeCached(s.ctx, trackID) {
		return
	}

	select {
	case s.queue <- trackID:
	case <-s.ctx.Done():
	}
}

func (s *EnrichmentService) requeueWithDelay(trackID string, delay time.Duration) {
	if trackID == "" {
		return
	}
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(delay):
	}
	s.enqueuedMu.Lock()
	delete(s.enqueuedMap, trackID)
	s.enqueuedMu.Unlock()
	s.Enqueue(trackID)
}

func (s *EnrichmentService) recordTransientRetry(ctx context.Context, trackID string, err error) {
	attempts := 0
	if s.db != nil {
		attempts, _ = s.db.GetEnrichmentAttempts(ctx, trackID)
	}
	const maxTransientAttempts = 5
	if attempts >= maxTransientAttempts {
		logEnrich.Warnf("Max transient retry attempts (%d) reached for track %s; marking as failed", attempts, trackID)
		if s.db != nil {
			_ = s.db.SaveEnrichmentState(ctx, trackID, "failed", attempts+1, fmt.Sprintf("max transient retries exceeded: %v", err), 24*time.Hour)
		}
		return
	}

	backoff := 30 * time.Second * time.Duration(1<<uint(attempts))
	if backoff > 10*time.Minute {
		backoff = 10 * time.Minute
	}
	logEnrich.Warnf("Transient error extracting FLAC metadata for track %s (attempt %d/%d, backoff %v): %v",
		trackID, attempts+1, maxTransientAttempts, backoff, err)
	if s.db != nil {
		_ = s.db.SaveEnrichmentState(ctx, trackID, "transient_retry", attempts+1, err.Error(), backoff)
	}
}

// CatchUpUnenriched enqueues tracks that lack artwork or have default metadata for background enrichment.
func (s *EnrichmentService) CatchUpUnenriched(ctx context.Context) {
	if s.db == nil {
		return
	}
	ids, err := s.db.ListUnenrichedTrackIDs(ctx, 20)
	if err != nil {
		logEnrich.Warnf("Failed to query unenriched tracks: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	logEnrich.Infof("Enqueuing %d tracks for background metadata enrichment...", len(ids))
	for _, id := range ids {
		s.Enqueue(id)
	}
}

func (s *EnrichmentService) workerLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		// 1. Wait if initial catchup sync is actively synchronizing
		s.mu.RLock()
		syncFn := s.isSyncing
		playFn := s.isPlaybackActive
		s.mu.RUnlock()

		if syncFn != nil && syncFn() {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// 2. Yield while playback has chunks pending or was read in the last few seconds
		if playFn != nil && playFn() {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// 3. Process from queue first (bounded queue with backpressure)
		select {
		case <-s.ctx.Done():
			return
		case trackID := <-s.queue:
			_ = s.EnrichTrack(s.ctx, trackID)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		default:
		}

		// 4. If queue is empty, drain the backlog in small batches (20 tracks)
		if s.db != nil {
			batch, err := s.db.ListUnenrichedTrackIDs(s.ctx, 20)
			if err == nil && len(batch) > 0 {
				for _, trackID := range batch {
					select {
					case <-s.ctx.Done():
						return
					default:
					}

					// Yield if playback or sync starts during batch processing
					s.mu.RLock()
					playFn = s.isPlaybackActive
					syncFn = s.isSyncing
					s.mu.RUnlock()

					for (playFn != nil && playFn()) || (syncFn != nil && syncFn()) {
						select {
						case <-s.ctx.Done():
							return
						case <-time.After(500 * time.Millisecond):
						}
						s.mu.RLock()
						playFn = s.isPlaybackActive
						syncFn = s.isSyncing
						s.mu.RUnlock()
					}

					// Check if a higher-priority direct request entered the queue
					select {
					case qTrackID := <-s.queue:
						_ = s.EnrichTrack(s.ctx, qTrackID)
					default:
					}

					_ = s.EnrichTrack(s.ctx, trackID)

					// Small pacing between tracks
					select {
					case <-s.ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
				continue
			}
		}

		// 5. Backlog is currently empty: wait for new queue items or idle poll
		select {
		case <-s.ctx.Done():
			return
		case trackID := <-s.queue:
			_ = s.EnrichTrack(s.ctx, trackID)
		case <-time.After(5 * time.Second):
		}
	}
}

// EnrichTrack extracts embedded tags and artwork from the audio file first,
// and falls back to online lookup only for fields still missing.
func (s *EnrichmentService) EnrichTrack(ctx context.Context, trackID string) error {
	if s.db == nil {
		return nil
	}

	// Confirm background reads yield to active sync or playback (Task d)
	s.mu.RLock()
	syncFn := s.isSyncing
	playFn := s.isPlaybackActive
	s.mu.RUnlock()

	if syncFn != nil && syncFn() {
		// Yield to sync: re-queue track to process after sync completes
		go s.requeueWithDelay(trackID, 2*time.Second)
		return nil
	}

	if playFn != nil && playFn() {
		// Yield to active playback: re-queue track to process when playback is idle
		go s.requeueWithDelay(trackID, 1*time.Second)
		return nil
	}

	track, err := s.db.GetTrack(ctx, trackID)
	if err != nil || track == nil {
		return err
	}

	s.mu.RLock()
	reader := s.reader
	artworkDir := s.artworkDir
	s.mu.RUnlock()

	var localArt string
	if artworkDir != "" {
		localArt = filepath.Join(artworkDir, trackID+".jpg")
	}

	staleArtOnDisk := false
	if localArt != "" {
		if fi, err := os.Stat(localArt); err == nil && fi.Size() > 0 {
			staleArtOnDisk = true
		}
	}

	artWrittenThisRun := false
	// 1. Always extract tags and artwork from the audio file first (authoritative source)
	var flacMeta *metadata.FLACMetadata
	if reader != nil {
		readCtx, readCancel := context.WithTimeout(models.WithBackground(ctx), 30*time.Second)
		meta, artWritten, extErr := metadata.ExtractEmbeddedMetadata(readCtx, trackID, reader, track.FileSize, artworkDir)
		readCancel()

		if extErr != nil {
			if errors.Is(extErr, metadata.ErrUnsupportedContainer) {
				logEnrich.Infof("Unsupported container for track %s: %v; falling back to online lookup without failed lock", trackID, extErr)
			} else if isTransientError(extErr) && !staleArtOnDisk {
				s.recordTransientRetry(ctx, trackID, extErr)
				return extErr
			} else {
				logEnrich.Infof("Failure or timeout extracting embedded metadata for track %s: %v; proceeding with fallback (staleArtOnDisk=%v)", trackID, extErr, staleArtOnDisk)
			}
		} else {
			flacMeta = meta
			if artWritten {
				artWrittenThisRun = true
				logEnrich.Infof("Track %s: embedded cover art successfully extracted and written this run (%s)", trackID, localArt)
			}
		}
	}

	hasDecodableArt := false
	if artWrittenThisRun {
		hasDecodableArt = true
	} else if staleArtOnDisk {
		if metadata.IsValidImage(localArt) {
			hasDecodableArt = true
			logEnrich.Infof("Track %s: verified valid existing embedded cover art on disk (%s)", trackID, localArt)
		} else {
			logEnrich.Warnf("Track %s: stale cover art on disk is corrupt or invalid image (%s); discarding", trackID, localArt)
			_ = os.Remove(localArt)
			if artworkDir != "" {
				_ = os.Remove(filepath.Join(artworkDir, trackID+".orig"))
			}
		}
	}

	var coverURL, thumbURL string
	var matchSource string
	var matchScore float64
	var metaVersion int = 3

	if hasDecodableArt {
		// Embedded art confirmed on disk -> serve via content:// provider URL (Bug 1 fix)
		coverURL = fmt.Sprintf("content://com.music.streamx.extensions.telegram.source/art/%s", trackID)
		thumbURL = coverURL
		matchSource = "embedded"
		matchScore = 1.0

		if flacMeta != nil {
			_ = s.db.SaveEmbeddedTrackMetadata(ctx, trackID, flacMeta, coverURL, thumbURL)
		} else {
			_ = s.db.UpdateTrackFullMetadata(ctx, trackID, track.Title, track.Artist, track.Album, coverURL, thumbURL, "", "", track.Year)
			_ = s.db.UpdateTrackMatchMetadata(ctx, trackID, matchSource, matchScore, metaVersion)
		}
		_ = s.db.SaveEnrichmentState(ctx, trackID, "success", 1, "embedded art and tags", 30*24*time.Hour)
		if artWrittenThisRun {
			logEnrich.Infof("Successfully enriched track %s from embedded audio file (art written this run)", trackID)
		} else {
			logEnrich.Infof("Successfully enriched track %s from embedded audio file (art validated from disk)", trackID)
		}
	} else {
		// No embedded picture found in the audio file:
		// Save embedded tags if extracted, then fall back to online cover art search
		if flacMeta != nil {
			_ = s.db.SaveEmbeddedTrackMetadata(ctx, trackID, flacMeta, "", "")
			if updated, err := s.db.GetTrack(ctx, trackID); err == nil && updated != nil {
				track = updated
			}
		}

		structMeta := metadata.CleanMetadataStructured(track.Title, track.Artist, track.Album, track.FileName)
		title := structMeta.Title
		artist := strings.Join(structMeta.Artists, ", ")
		album := track.Album
		if structMeta.AlbumHint != "" && (album == "" || album == "Telegram Music") {
			album = structMeta.AlbumHint
		}
		year := track.Year
		durationSec := track.DurationSec

		cand, score, _, reason, err := s.FindCoverScored(ctx, structMeta, durationSec)
		if err == nil && cand != nil && score >= 0.70 {
			coverURL = cand.ImageURL
			thumbURL = cand.ImageURL
			matchSource = cand.Provider
			matchScore = score
			if cand.Album != "" && (album == "" || album == "Telegram Music") {
				album = cand.Album
			}
			if cand.Year > 0 && year == 0 {
				year = cand.Year
			}
		} else {
			matchSource = "none"
			matchScore = 0.0
			logEnrich.Infof("No confident cover match for %s: %s (reason: %s)", trackID, title, reason)
		}

		if coverURL != "" {
			_ = s.db.UpdateTrackFullMetadata(ctx, trackID, title, artist, album, coverURL, thumbURL, "", "", year)
			_ = s.db.UpdateTrackMatchMetadata(ctx, trackID, matchSource, matchScore, metaVersion)
			_ = s.db.SaveEnrichmentState(ctx, trackID, "success", 1, "matched "+matchSource, 30*24*time.Hour)
			logEnrich.Infof("Successfully enriched track %s with online cover (%s, score: %.2f)", trackID, matchSource, matchScore)
		} else {
			var fallbackArt string
			if artworkDir != "" {
				artPath := filepath.Join(artworkDir, trackID+".jpg")
				if fi, err := os.Stat(artPath); err == nil && fi.Size() > 0 {
					fallbackArt = fmt.Sprintf("content://com.music.streamx.extensions.telegram.source/art/%s", trackID)
				}
			}
			_ = s.db.UpdateTrackFullMetadata(ctx, trackID, title, artist, album, fallbackArt, fallbackArt, "", "", year)
			_ = s.db.UpdateTrackMatchMetadata(ctx, trackID, "none", 0.0, metaVersion)
			_ = s.db.SaveEnrichmentState(ctx, trackID, "failed", 1, "no confident cover match", 24*time.Hour)
		}
	}

	// 2. Lyrics retrieval
	if track.Lyrics == "" && track.Title != "" {
		firstArtist := track.Artist
		if flacMeta != nil && len(flacMeta.Artists) > 0 {
			firstArtist = flacMeta.Artists[0]
		}
		res, err := s.FindLyrics(ctx, track.Title, firstArtist)
		if err == nil && res != nil {
			_ = s.db.UpdateTrackFullMetadata(ctx, trackID, "", "", "", "", "", res.PlainLyrics, res.SyncedLyrics, 0)
		}
	}

	return nil
}

// FindCoverScored runs external candidate retrieval across providers (JioSaavn, iTunes IN/US, Deezer, MusicBrainz),
// evaluates candidates using ScoreCandidate against structured metadata, and picks the highest scoring candidate above threshold.
func (s *EnrichmentService) FindCoverScored(ctx context.Context, local metadata.StructuredMetadata, localDurationSec int) (*CoverCandidate, float64, string, string, error) {
	type providerSearcher struct {
		name string
		fn   func() ([]CoverCandidate, []string, error)
	}

	searchers := []providerSearcher{
		{
			name: "jiosaavn",
			fn: func() ([]CoverCandidate, []string, error) {
				return s.searchJioSaavnCandidates(ctx, local.SearchTitle, local.SearchArtists)
			},
		},
		{
			name: "itunes",
			fn: func() ([]CoverCandidate, []string, error) {
				return s.searchITunesCandidates(ctx, local.SearchTitle, local.SearchArtists)
			},
		},
		{
			name: "deezer",
			fn: func() ([]CoverCandidate, []string, error) {
				return s.searchDeezerCandidates(ctx, local.SearchTitle, local.SearchArtists)
			},
		},
		{
			name: "musicbrainz",
			fn: func() ([]CoverCandidate, []string, error) {
				return s.searchMusicBrainzCandidates(ctx, local.SearchTitle, local.SearchArtists)
			},
		},
	}

	var allScored []CandidateScore
	for _, searcher := range searchers {
		cands, _, err := searcher.fn()
		if err != nil || len(cands) == 0 {
			continue
		}

		for _, c := range cands {
			score, rule, reason := ScoreCandidate(local, localDurationSec, c)
			if score > 0 {
				allScored = append(allScored, CandidateScore{
					Score:     score,
					Rule:      rule,
					Reason:    reason,
					Candidate: c,
				})
			}
		}

		if len(allScored) > 0 {
			sort.Slice(allScored, func(i, j int) bool {
				return allScored[i].Score > allScored[j].Score
			})
			if allScored[0].Score >= 0.85 {
				best := allScored[0]
				return &best.Candidate, best.Score, best.Rule, best.Reason, nil
			}
		}
	}

	if len(allScored) == 0 {
		return nil, 0, "none", "no candidates met threshold", fmt.Errorf("no candidate accepted")
	}

	sort.Slice(allScored, func(i, j int) bool {
		return allScored[i].Score > allScored[j].Score
	})

	var runnerUp float64
	if len(allScored) > 1 {
		runnerUp = allScored[1].Score
	}

	best := allScored[0]
	finalScore, finalRule, finalReason := ScoreCandidate(local, localDurationSec, best.Candidate, runnerUp)
	if finalScore > 0 {
		return &best.Candidate, finalScore, finalRule, finalReason, nil
	}

	return nil, 0, "below_threshold", finalReason, fmt.Errorf("no candidate accepted")
}

func (s *EnrichmentService) FindCover(ctx context.Context, title, artist, album string) (coverURL, thumbURL, foundAlbum string, year int, err error) {
	structMeta := metadata.CleanMetadataStructured(title, artist, album, "")
	cand, _, _, reason, err := s.FindCoverScored(ctx, structMeta, 0)
	if err != nil || cand == nil {
		return "", "", "", 0, fmt.Errorf("no cover found: %s", reason)
	}
	return cand.ImageURL, cand.ImageURL, cand.Album, cand.Year, nil
}

func coreSongTitle(title string) string {
	t := title
	delims := []string{" (From ", " [From ", " - From ", " (feat.", " [feat.", " (Reprise", " (Male", " (Female", " [Title", " (Title"}
	for _, d := range delims {
		if idx := strings.Index(strings.ToLower(t), strings.ToLower(d)); idx > 0 {
			t = t[:idx]
		}
	}
	return strings.TrimSpace(t)
}

func splitArtists(artistStr string) []string {
	clean := metadata.StripNoise(artistStr)
	replacer := strings.NewReplacer(
		" / ", ",",
		" & ", ",",
		" and ", ",",
		" ft. ", ",",
		" feat. ", ",",
		" feat ", ",",
		";", ",",
	)
	normalized := replacer.Replace(clean)
	parts := strings.Split(normalized, ",")
	var result []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if len(trimmed) >= 2 && !strings.EqualFold(trimmed, "Unknown Artist") {
			result = append(result, trimmed)
		}
	}
	return result
}

// SplitArtists splits multi-artist strings using standard delimiters and feature tags.
func SplitArtists(artistStr string) []string {
	return splitArtists(artistStr)
}

func parseDuration(v interface{}) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(val))
		return n
	default:
		return 0
	}
}

type jioSaavnArtistItem struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type jioSaavnResult struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Subtitle string      `json:"subtitle"`
	Type     string      `json:"type"`
	Image    string      `json:"image"`
	Year     string      `json:"year"`
	Album    string      `json:"album"`
	Singers  string      `json:"singers"`
	Duration interface{} `json:"duration"`
	MoreInfo struct {
		Album     string                `json:"album"`
		Music     string                `json:"music"`
		Duration  interface{}           `json:"duration"`
		ArtistMap struct {
			PrimaryArtists  []jioSaavnArtistItem `json:"primary_artists"`
			FeaturedArtists []jioSaavnArtistItem `json:"featured_artists"`
			Artists         []jioSaavnArtistItem `json:"artists"`
		} `json:"artistMap"`
	} `json:"more_info"`
}

type jioSaavnResponse struct {
	Total   int              `json:"total"`
	Results []jioSaavnResult `json:"results"`
}

func parseYear(v interface{}) int {
	if v == nil {
		return 0
	}
	switch s := v.(type) {
	case int:
		return s
	case float64:
		return int(s)
	case string:
		s = strings.TrimSpace(s)
		if len(s) >= 4 {
			y, _ := strconv.Atoi(s[:4])
			return y
		}
	}
	return 0
}

func (s *EnrichmentService) searchJioSaavnCandidates(ctx context.Context, title string, artists []string) ([]CoverCandidate, []string, error) {
	if s.jsLimiter != nil && !s.jsLimiter.Allow() {
		return nil, nil, fmt.Errorf("jiosaavn: rate limited")
	}

	cleanT := metadata.StripNoise(title)
	coreT := coreSongTitle(cleanT)

	queries := make([]string, 0, 4)
	if len(artists) > 0 {
		queries = append(queries, fmt.Sprintf("%s %s", cleanT, artists[0]))
		queries = append(queries, fmt.Sprintf("%s %s", artists[0], cleanT))
		if len(artists) > 1 {
			queries = append(queries, fmt.Sprintf("%s %s", cleanT, artists[1]))
		}
		if coreT != "" && coreT != cleanT {
			queries = append(queries, fmt.Sprintf("%s %s", coreT, artists[0]))
		}
	} else {
		queries = append(queries, cleanT)
		if coreT != "" && coreT != cleanT {
			queries = append(queries, coreT)
		}
	}

	var candidates []CoverCandidate
	for _, q := range queries {
		apiURL := fmt.Sprintf("https://www.jiosaavn.com/api.php?__call=search.getResults&_format=json&_marker=0&api_version=4&ctx=web6dot0&q=%s&p=1&n=10", url.QueryEscape(q))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if s.jsLimiter != nil {
				s.jsLimiter.Mark429(30 * time.Second)
			}
			resp.Body.Close()
			return candidates, queries, fmt.Errorf("jiosaavn 429")
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}

		var payload jioSaavnResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil || len(payload.Results) == 0 {
			continue
		}

		for _, r := range payload.Results {
			if r.Type != "" && r.Type != "song" {
				continue
			}
			dur := parseDuration(r.MoreInfo.Duration)
			if dur == 0 {
				dur = parseDuration(r.Duration)
			}

			rawImg := strings.TrimSpace(r.Image)
			highRes := strings.Replace(rawImg, "150x150", "500x500", 1)
			highRes = strings.Replace(highRes, "50x50", "500x500", 1)
			if strings.HasPrefix(highRes, "http://") {
				highRes = "https://" + strings.TrimPrefix(highRes, "http://")
			}

			var candArtists []string
			if r.Singers != "" {
				candArtists = append(candArtists, splitArtists(r.Singers)...)
			}
			for _, a := range r.MoreInfo.ArtistMap.PrimaryArtists {
				candArtists = append(candArtists, html.UnescapeString(a.Name))
			}
			for _, a := range r.MoreInfo.ArtistMap.Artists {
				candArtists = append(candArtists, html.UnescapeString(a.Name))
			}

			alb := strings.TrimSpace(html.UnescapeString(r.MoreInfo.Album))
			if alb == "" {
				alb = strings.TrimSpace(html.UnescapeString(r.Album))
			}

			y := parseYear(r.Year)

			candidates = append(candidates, CoverCandidate{
				Title:       html.UnescapeString(r.Title),
				Artists:     candArtists,
				Album:       alb,
				Year:        y,
				DurationSec: dur,
				ImageURL:    highRes,
				Provider:    "jiosaavn",
			})
		}
		if len(candidates) > 0 {
			break
		}
	}

	return candidates, queries, nil
}

func (s *EnrichmentService) searchJioSaavn(ctx context.Context, title, artist string) (coverURL, thumbURL, album string, year int, err error) {
	artists := splitArtists(artist)
	cands, _, err := s.searchJioSaavnCandidates(ctx, title, artists)
	if err != nil {
		return "", "", "", 0, err
	}
	structMeta := metadata.CleanMetadataStructured(title, artist, "", "")
	var bestCand *CoverCandidate
	var bestScore float64
	for _, c := range cands {
		score, _, _ := ScoreCandidate(structMeta, 0, c)
		if score > bestScore {
			bestScore = score
			cc := c
			bestCand = &cc
		}
	}
	if bestCand != nil && bestScore >= 0.70 {
		return bestCand.ImageURL, bestCand.ImageURL, bestCand.Album, bestCand.Year, nil
	}
	return "", "", "", 0, fmt.Errorf("jiosaavn: no cover found for %s - %s", artist, title)
}

func (s *EnrichmentService) SearchJioSaavn(ctx context.Context, title, artist string) (coverURL, thumbURL, album string, year int, err error) {
	return s.searchJioSaavn(ctx, title, artist)
}

func (s *EnrichmentService) searchITunesCandidates(ctx context.Context, title string, artists []string) ([]CoverCandidate, []string, error) {
	if s.itLimiter != nil && !s.itLimiter.Allow() {
		return nil, nil, fmt.Errorf("itunes: rate limited")
	}

	cleanT := metadata.StripNoise(title)
	storefronts := []string{"IN", ""} // Storefront IN first, then worldwide fallback
	queries := make([]string, 0, 4)

	if len(artists) > 0 {
		queries = append(queries, fmt.Sprintf("%s %s", cleanT, artists[0]))
		queries = append(queries, fmt.Sprintf("%s %s", artists[0], cleanT))
		if len(artists) > 1 {
			queries = append(queries, fmt.Sprintf("%s %s", cleanT, artists[1]))
		}
	} else if cleanT != "" {
		queries = append(queries, cleanT)
	}

	var candidates []CoverCandidate
	for _, q := range queries {
		for _, sf := range storefronts {
			apiURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=song&limit=10", url.QueryEscape(q))
			if sf != "" {
				apiURL += "&country=" + sf
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
			if err != nil {
				continue
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

			resp, err := s.httpClient.Do(req)
			if err != nil {
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				if s.itLimiter != nil {
					s.itLimiter.Mark429(30 * time.Second)
				}
				resp.Body.Close()
				return candidates, queries, fmt.Errorf("itunes 429")
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				continue
			}

			var payload struct {
				ResultCount int `json:"resultCount"`
				Results     []struct {
					TrackName       string  `json:"trackName"`
					ArtistName      string  `json:"artistName"`
					CollectionName  string  `json:"collectionName"`
					ArtworkUrl100   string  `json:"artworkUrl100"`
					ReleaseDate     string  `json:"releaseDate"`
					TrackTimeMillis float64 `json:"trackTimeMillis"`
				} `json:"results"`
			}

			decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if decodeErr != nil || len(payload.Results) == 0 {
				continue
			}

			for _, r := range payload.Results {
				durSec := int(r.TrackTimeMillis / 1000)
				highRes := strings.Replace(r.ArtworkUrl100, "100x100bb.jpg", "1000x1000bb.jpg", 1)
				y := parseYear(r.ReleaseDate)

				candidates = append(candidates, CoverCandidate{
					Title:       r.TrackName,
					Artists:     []string{r.ArtistName},
					Album:       r.CollectionName,
					Year:        y,
					DurationSec: durSec,
					ImageURL:    highRes,
					Provider:    "itunes",
				})
			}
			if len(candidates) > 0 {
				break
			}
		}
		if len(candidates) > 0 {
			break
		}
	}

	return candidates, queries, nil
}

func (s *EnrichmentService) searchITunes(ctx context.Context, title, artist string) (coverURL, thumbURL string, year int, err error) {
	artists := splitArtists(artist)
	cands, _, err := s.searchITunesCandidates(ctx, title, artists)
	if err != nil {
		return "", "", 0, err
	}
	structMeta := metadata.CleanMetadataStructured(title, artist, "", "")
	var bestCand *CoverCandidate
	var bestScore float64
	for _, c := range cands {
		score, _, _ := ScoreCandidate(structMeta, 0, c)
		if score > bestScore {
			bestScore = score
			cc := c
			bestCand = &cc
		}
	}
	if bestCand != nil && bestScore >= 0.70 {
		return bestCand.ImageURL, bestCand.ImageURL, bestCand.Year, nil
	}
	return "", "", 0, fmt.Errorf("itunes: no cover found for %s - %s", artist, title)
}

func (s *EnrichmentService) SearchITunes(ctx context.Context, title, artist string) (coverURL, thumbURL string, year int, err error) {
	return s.searchITunes(ctx, title, artist)
}

func (s *EnrichmentService) searchDeezerCandidates(ctx context.Context, title string, artists []string) ([]CoverCandidate, []string, error) {
	if s.dzLimiter != nil && !s.dzLimiter.Allow() {
		return nil, nil, fmt.Errorf("deezer: rate limited")
	}

	cleanT := metadata.StripNoise(title)
	queries := make([]string, 0, 2)
	if len(artists) > 0 {
		queries = append(queries, fmt.Sprintf("artist:\"%s\" track:\"%s\"", artists[0], cleanT))
	} else {
		queries = append(queries, cleanT)
	}

	var candidates []CoverCandidate
	for _, q := range queries {
		apiURL := fmt.Sprintf("https://api.deezer.com/search?q=%s&limit=10", url.QueryEscape(q))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if s.dzLimiter != nil {
				s.dzLimiter.Mark429(30 * time.Second)
			}
			resp.Body.Close()
			return candidates, queries, fmt.Errorf("deezer 429")
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}

		var payload struct {
			Data []struct {
				Title    string `json:"title"`
				Duration int    `json:"duration"`
				Artist   struct {
					Name string `json:"name"`
				} `json:"artist"`
				Album struct {
					Title       string `json:"title"`
					CoverXL     string `json:"cover_xl"`
					CoverBig    string `json:"cover_big"`
					CoverMedium string `json:"cover_medium"`
				} `json:"album"`
			} `json:"data"`
		}

		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil || len(payload.Data) == 0 {
			continue
		}

		for _, item := range payload.Data {
			img := item.Album.CoverXL
			if img == "" {
				img = item.Album.CoverBig
			}
			if img == "" {
				img = item.Album.CoverMedium
			}
			if img == "" {
				continue
			}

			candidates = append(candidates, CoverCandidate{
				Title:       item.Title,
				Artists:     []string{item.Artist.Name},
				Album:       item.Album.Title,
				DurationSec: item.Duration,
				ImageURL:    img,
				Provider:    "deezer",
			})
		}
		if len(candidates) > 0 {
			break
		}
	}

	return candidates, queries, nil
}

func (s *EnrichmentService) SearchDeezer(ctx context.Context, title, artist string) (coverURL, thumbURL string, err error) {
	artists := splitArtists(artist)
	cands, _, err := s.searchDeezerCandidates(ctx, title, artists)
	if err != nil {
		return "", "", err
	}
	structMeta := metadata.CleanMetadataStructured(title, artist, "", "")
	var bestCand *CoverCandidate
	var bestScore float64
	for _, c := range cands {
		score, _, _ := ScoreCandidate(structMeta, 0, c)
		if score > bestScore {
			bestScore = score
			cc := c
			bestCand = &cc
		}
	}
	if bestCand != nil && bestScore >= 0.70 {
		return bestCand.ImageURL, bestCand.ImageURL, nil
	}
	return "", "", fmt.Errorf("deezer: no cover found for %s - %s", artist, title)
}

func (s *EnrichmentService) searchMusicBrainzCandidates(ctx context.Context, title string, artists []string) ([]CoverCandidate, []string, error) {
	if s.mbLimiter != nil && !s.mbLimiter.Allow() {
		return nil, nil, fmt.Errorf("musicbrainz: rate limited")
	}

	cleanT := metadata.StripNoise(title)
	if cleanT == "" {
		return nil, nil, nil
	}

	var query string
	if len(artists) > 0 && artists[0] != "" {
		query = fmt.Sprintf("recording:\"%s\" AND artist:\"%s\"", cleanT, artists[0])
	} else {
		query = fmt.Sprintf("recording:\"%s\"", cleanT)
	}

	apiURL := fmt.Sprintf("https://musicbrainz.org/ws/2/recording/?query=%s&fmt=json&limit=5", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, []string{query}, err
	}
	req.Header.Set("User-Agent", "StreamXTelegram/1.0 (contact@burhanverse.eu.org)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, []string{query}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		if s.mbLimiter != nil {
			s.mbLimiter.Mark429(30 * time.Second)
		}
		return nil, []string{query}, fmt.Errorf("musicbrainz 429")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, []string{query}, fmt.Errorf("musicbrainz status: %d", resp.StatusCode)
	}

	var payload struct {
		Recordings []struct {
			Title        string `json:"title"`
			Length       int    `json:"length"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
			Releases []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Date  string `json:"date"`
			} `json:"releases"`
		} `json:"recordings"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, []string{query}, err
	}

	var candidates []CoverCandidate
	for _, rec := range payload.Recordings {
		if len(rec.Releases) == 0 || rec.Releases[0].ID == "" {
			continue
		}
		var artistNames []string
		for _, ac := range rec.ArtistCredit {
			if ac.Name != "" {
				artistNames = append(artistNames, ac.Name)
			}
		}
		releaseID := rec.Releases[0].ID
		imageURL := fmt.Sprintf("https://coverartarchive.org/release/%s/front-500", releaseID)
		year := 0
		if len(rec.Releases[0].Date) >= 4 {
			year, _ = strconv.Atoi(rec.Releases[0].Date[:4])
		}
		durSec := rec.Length / 1000

		candidates = append(candidates, CoverCandidate{
			Title:       rec.Title,
			Artists:     artistNames,
			Album:       rec.Releases[0].Title,
			Year:        year,
			DurationSec: durSec,
			ImageURL:    imageURL,
			Provider:    "musicbrainz",
		})
	}

	return candidates, []string{query}, nil
}

// FindLyrics searches LRCLIB for plain or synchronized (LRC) lyrics.
func (s *EnrichmentService) FindLyrics(ctx context.Context, title, artist string) (*LyricsResult, error) {
	trackArtists := splitArtists(artist)
	queries := []string{artist}
	if len(trackArtists) > 0 && trackArtists[0] != artist {
		queries = append(queries, trackArtists[0])
	}

	for _, a := range queries {
		apiURL := fmt.Sprintf("https://lrclib.net/api/get?track_name=%s&artist_name=%s",
			url.QueryEscape(title), url.QueryEscape(a))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "StreamX/1.0")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var payload struct {
				PlainLyrics  string `json:"plainLyrics"`
				SyncedLyrics string `json:"syncedLyrics"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && (payload.PlainLyrics != "" || payload.SyncedLyrics != "") {
				resp.Body.Close()
				return &LyricsResult{
					PlainLyrics:  payload.PlainLyrics,
					SyncedLyrics: payload.SyncedLyrics,
					Source:       "lrclib",
				}, nil
			}
		}
		resp.Body.Close()
	}

	return nil, nil
}
