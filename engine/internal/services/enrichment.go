package services

import (
	"context"
	"encoding/json"
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

// CandidateItem represents candidate details from an external music metadata provider.
type CandidateItem struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	DurationSec int    `json:"duration_sec"`
}

// ProviderLog captures queries sent and candidates returned for a specific provider.
type ProviderLog struct {
	Queries    []string        `json:"queries"`
	Candidates []CandidateItem `json:"candidates"`
}

// DecisionLogDetails records the accepted result and rule used.
type DecisionLogDetails struct {
	Provider          string         `json:"provider"` // "jiosaavn", "itunes", "deezer", "embedded", "none"
	Rule              string         `json:"rule"`     // "exact", "partial", "fallback", "embedded", "none"
	AcceptedCandidate *CandidateItem `json:"accepted_candidate,omitempty"`
}

// EnrichmentRecord represents one track enrichment log entry.
type EnrichmentRecord struct {
	Timestamp     string                 `json:"timestamp"`
	TrackID       string                 `json:"track_id"`
	RawTags       map[string]string      `json:"raw_tags"`
	RawFilename   string                 `json:"raw_filename"`
	Cleaned       map[string]string      `json:"cleaned"`
	QueriesSent   []string               `json:"queries_sent"`
	Providers     map[string]ProviderLog `json:"providers"`
	Decision      DecisionLogDetails     `json:"decision"`
	FinalCoverURL string                 `json:"final_cover_url"`
}

type enrichmentRotatedFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
}

func newEnrichmentRotatedFile(path string, maxBytes int64) *enrichmentRotatedFile {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	return &enrichmentRotatedFile{
		path:     path,
		maxBytes: maxBytes,
	}
}

func (rf *enrichmentRotatedFile) Append(data []byte) error {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	fi, err := os.Stat(rf.path)
	if err == nil && fi.Size()+int64(len(data))+1 > rf.maxBytes {
		backupPath := rf.path + ".1"
		_ = os.Remove(backupPath)
		_ = os.Rename(rf.path, backupPath)
	}

	f, err := os.OpenFile(rf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return err
	}
	_, err = f.Write([]byte("\n"))
	return err
}

type enrichmentRingBuffer struct {
	mu       sync.RWMutex
	capacity int
	lines    []string
}

func newEnrichmentRingBuffer(capacity int) *enrichmentRingBuffer {
	return &enrichmentRingBuffer{
		capacity: capacity,
		lines:    make([]string, 0, capacity),
	}
}

func (rb *enrichmentRingBuffer) Add(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.lines) >= rb.capacity {
		rb.lines = rb.lines[1:]
	}
	rb.lines = append(rb.lines, line)
}

func (rb *enrichmentRingBuffer) Get(limit int) []string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	n := len(rb.lines)
	if limit <= 0 || limit > n {
		limit = n
	}
	start := n - limit
	result := make([]string, limit)
	copy(result, rb.lines[start:])
	return result
}

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
	coverCache       sync.Map
	debugEnabled     bool
	enrichLogFile    *enrichmentRotatedFile
	enrichRingBuffer *enrichmentRingBuffer

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

// GetCachedCover returns the in-memory cached HTTPS artwork URL for a title and artist if available.
func (s *EnrichmentService) GetCachedCover(title, artist string) string {
	cleanTitle := metadata.StripNoise(title)
	cleanArtist := metadata.StripNoise(artist)
	key := strings.ToLower(fmt.Sprintf("%s - %s", cleanArtist, cleanTitle))
	if val, ok := s.coverCache.Load(key); ok {
		if str, ok := val.(string); ok && strings.HasPrefix(str, "https://") {
			return str
		}
	}
	return ""
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

// SetDebug configures debug diagnostics and output log directory.
func (s *EnrichmentService) SetDebug(enabled bool, filesDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debugEnabled = enabled
	if enabled && filesDir != "" {
		logPath := filepath.Join(filesDir, "logs", "enrich.jsonl")
		s.enrichLogFile = newEnrichmentRotatedFile(logPath, 5*1024*1024)
		s.enrichRingBuffer = newEnrichmentRingBuffer(50)
	}
}

// GetRecentEnrichmentLogs returns the most recent enrichment decision lines up to limit.
func (s *EnrichmentService) GetRecentEnrichmentLogs(limit int) []string {
	s.mu.RLock()
	rb := s.enrichRingBuffer
	s.mu.RUnlock()
	if rb == nil {
		return nil
	}
	return rb.Get(limit)
}

func (s *EnrichmentService) logEnrichmentRecord(rec EnrichmentRecord) {
	s.mu.RLock()
	enabled := s.debugEnabled
	file := s.enrichLogFile
	rb := s.enrichRingBuffer
	s.mu.RUnlock()

	if !enabled {
		return
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line := string(data)

	if rb != nil {
		rb.Add(line)
	}
	if file != nil {
		_ = file.Append(data)
	}
}

// Stop gracefully stops the background enrichment queue.
func (s *EnrichmentService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Enqueue adds a track ID to the background enrichment queue, deduplicating and skipping negative cached entries.
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
	default:
		logEnrich.Warnf("Enrichment queue full; dropping track %s", trackID)
	}
}

// CatchUpUnenriched enqueues all tracks that lack artwork or have default metadata for background enrichment.
func (s *EnrichmentService) CatchUpUnenriched(ctx context.Context) {
	if s.db == nil {
		return
	}
	ids, err := s.db.ListUnenrichedTrackIDs(ctx)
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
		lastPlay := s.lastPlaybackSeen
		s.mu.RUnlock()

		if syncFn != nil && syncFn() {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// 2. Pause entirely while any playback stream is active
		if playFn != nil && playFn() {
			s.mu.Lock()
			s.lastPlaybackSeen = time.Now()
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// 3. Wait for short idle period (2s) after playback ends
		if !lastPlay.IsZero() && time.Since(lastPlay) < 2*time.Second {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		select {
		case <-s.ctx.Done():
			return
		case trackID := <-s.queue:
			_ = s.EnrichTrack(s.ctx, trackID)
			// Small pacing between requests to avoid burst rate-limits
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// EnrichTrack fetches embedded artwork & tags, remote artwork fallback, and lyrics for a specific track.
func (s *EnrichmentService) EnrichTrack(ctx context.Context, trackID string) error {
	if s.db == nil {
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

	// Intelligently extract clean title, artist, album, version tags from tags and Telegram filename
	structMeta := metadata.CleanMetadataStructured(track.Title, track.Artist, track.Album, track.FileName)
	title := structMeta.Title
	artist := strings.Join(structMeta.Artists, ", ")
	album := track.Album
	if structMeta.AlbumHint != "" && (album == "" || album == "Telegram Music") {
		album = structMeta.AlbumHint
	}
	year := track.Year
	durationSec := track.DurationSec

	var localArt string
	if artworkDir != "" {
		localArt = filepath.Join(artworkDir, trackID+".jpg")
	}

	// 1. Check embedded cover art from local file
	hasEmbeddedPic := false
	if localArt != "" {
		if fi, err := os.Stat(localArt); err == nil && fi.Size() > 0 {
			hasEmbeddedPic = true
		}
	}

	// 1b. If not on disk, try reading headers over MTProto via ReadHeader (lowest priority, no prefetch)
	hasFreeCleanData := track.Title != "" && !strings.HasPrefix(track.Title, "Track ") &&
		track.Artist != "" && track.Artist != "Unknown Artist" &&
		track.DurationSec > 0

	var headerMeta *metadata.AudioHeaderMeta
	if !hasEmbeddedPic && reader != nil && !hasFreeCleanData {
		readCtx, readCancel := context.WithTimeout(models.WithBackground(ctx), 4*time.Second)
		var chunk []byte
		var err error
		if hr, ok := reader.(HeaderReader); ok {
			chunk, err = hr.ReadHeader(readCtx, trackID, 256*1024)
		} else {
			chunk, err = reader.ReadChunk(readCtx, trackID, 0, 256*1024)
		}
		readCancel()
		if err == nil && len(chunk) > 0 {
			headerMeta = metadata.ParseAudioHeader(chunk)
			if headerMeta != nil {
				if len(headerMeta.Picture) > 0 && localArt != "" {
					_ = os.WriteFile(localArt, headerMeta.Picture, 0644)
					logEnrich.Infof("Extracted embedded cover art (%d bytes) for track %s", len(headerMeta.Picture), trackID)
					hasEmbeddedPic = true
				}
				if headerMeta.Year > 0 && year == 0 {
					year = headerMeta.Year
				}
			}
		}
	}

	var coverURL, thumbURL string
	var matchSource string
	var matchScore float64
	var metaVersion int = 2
	var decision DecisionLogDetails
	provLogs := make(map[string]ProviderLog)
	var allQueriesSent []string

	if hasEmbeddedPic {
		// Source order (a): embedded art via content:// provider (Prompt 4 requirement)
		coverURL = fmt.Sprintf("content://com.music.streamx.extensions.telegram.source/art/%s", trackID)
		thumbURL = coverURL
		matchSource = "embedded"
		matchScore = 1.0
		decision = DecisionLogDetails{
			Provider: "embedded",
			Rule:     "exact",
		}
	} else {
		// Source order (b): online lookup only when no embedded art
		durationBucket := durationSec / 10
		cacheKey := strings.ToLower(fmt.Sprintf("%s|%s|%d|%s", strings.Join(structMeta.SearchArtists, ","), structMeta.SearchTitle, durationBucket, structMeta.AlbumHint))
		if val, ok := s.coverCache.Load(cacheKey); ok {
			coverURL = val.(string)
			thumbURL = coverURL
			matchSource = "cache"
			matchScore = 0.90
			decision = DecisionLogDetails{
				Provider: "cache",
				Rule:     "exact",
			}
		} else {
			cand, score, rule, reason, plogs, queries, err := s.FindCoverScored(ctx, structMeta, durationSec)
			provLogs = plogs
			allQueriesSent = queries
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
				s.coverCache.Store(cacheKey, coverURL)
				decision = DecisionLogDetails{
					Provider: cand.Provider,
					Rule:     rule,
					AcceptedCandidate: &CandidateItem{
						Title:       cand.Title,
						Artist:      strings.Join(cand.Artists, ", "),
						DurationSec: cand.DurationSec,
					},
				}
			} else {
				// Prefer no cover over a wrong cover
				matchSource = "none"
				matchScore = 0.0
				decision = DecisionLogDetails{
					Provider: "none",
					Rule:     rule,
				}
				logEnrich.Infof("No confident cover match for %s: %s (reason: %s)", trackID, title, reason)
			}
		}
	}

	// 3. Lyrics retrieval
	var plainLyrics, syncedLyrics string
	if track.Lyrics == "" && title != "" {
		firstArtist := ""
		if len(structMeta.Artists) > 0 {
			firstArtist = structMeta.Artists[0]
		}
		res, err := s.FindLyrics(ctx, title, firstArtist)
		if err == nil && res != nil {
			plainLyrics = res.PlainLyrics
			syncedLyrics = res.SyncedLyrics
		}
	}

	// 4. Persist enrichment in DB
	if coverURL != "" {
		_ = s.db.UpdateTrackFullMetadata(ctx, trackID, title, artist, album, coverURL, thumbURL, plainLyrics, syncedLyrics, year)
		_ = s.db.UpdateTrackMatchMetadata(ctx, trackID, matchSource, matchScore, metaVersion)
		_ = s.db.SaveEnrichmentState(ctx, trackID, "success", 1, "matched "+matchSource, 30*24*time.Hour)
		logEnrich.Infof("Successfully enriched track %s: %s - %s (source: %s, score: %.2f)", trackID, artist, title, matchSource, matchScore)
	} else {
		_ = s.db.UpdateTrackFullMetadata(ctx, trackID, title, artist, album, "", "", plainLyrics, syncedLyrics, year)
		_ = s.db.UpdateTrackMatchMetadata(ctx, trackID, "none", 0.0, metaVersion)
		_ = s.db.SaveEnrichmentState(ctx, trackID, "failed", 1, "no confident cover match", 24*time.Hour)
	}

	s.logEnrichmentRecord(EnrichmentRecord{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		TrackID:   trackID,
		RawTags: map[string]string{
			"title":  track.Title,
			"artist": track.Artist,
			"album":  track.Album,
		},
		RawFilename: track.FileName,
		Cleaned: map[string]string{
			"title":  title,
			"artist": artist,
		},
		QueriesSent:   allQueriesSent,
		Providers:     provLogs,
		Decision:      decision,
		FinalCoverURL: coverURL,
	})

	return nil
}

// FindCoverScored runs external candidate retrieval across providers (JioSaavn, iTunes IN/US, Deezer, MusicBrainz),
// evaluates candidates using ScoreCandidate against structured metadata, and picks the highest scoring candidate above threshold.
func (s *EnrichmentService) FindCoverScored(ctx context.Context, local metadata.StructuredMetadata, localDurationSec int) (*CoverCandidate, float64, string, string, map[string]ProviderLog, []string, error) {
	provLogs := make(map[string]ProviderLog)
	var allQueries []string

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
		cands, queries, err := searcher.fn()
		allQueries = append(allQueries, queries...)

		var logCands []CandidateItem
		for _, c := range cands {
			logCands = append(logCands, CandidateItem{
				Title:       c.Title,
				Artist:      strings.Join(c.Artists, ", "),
				DurationSec: c.DurationSec,
			})
		}
		provLogs[searcher.name] = ProviderLog{
			Queries:    queries,
			Candidates: logCands,
		}

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
				return &best.Candidate, best.Score, best.Rule, best.Reason, provLogs, allQueries, nil
			}
		}
	}

	if len(allScored) == 0 {
		return nil, 0, "none", "no candidates met threshold", provLogs, allQueries, fmt.Errorf("no candidate accepted")
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
		return &best.Candidate, finalScore, finalRule, finalReason, provLogs, allQueries, nil
	}

	return nil, 0, "below_threshold", finalReason, provLogs, allQueries, fmt.Errorf("no candidate accepted")
}

func (s *EnrichmentService) FindCoverWithDecision(ctx context.Context, title, artist, album string) (coverURL, thumbURL, foundAlbum string, year int, decision DecisionLogDetails, provLogs map[string]ProviderLog, queriesSent []string, err error) {
	structMeta := metadata.CleanMetadataStructured(title, artist, album, "")
	cand, _, rule, reason, plogs, qSent, err := s.FindCoverScored(ctx, structMeta, 0)
	if err != nil || cand == nil {
		decision = DecisionLogDetails{
			Provider: "none",
			Rule:     rule,
		}
		return "", "", "", 0, decision, plogs, qSent, fmt.Errorf("no cover found: %s", reason)
	}

	decision = DecisionLogDetails{
		Provider: cand.Provider,
		Rule:     rule,
		AcceptedCandidate: &CandidateItem{
			Title:       cand.Title,
			Artist:      strings.Join(cand.Artists, ", "),
			DurationSec: cand.DurationSec,
		},
	}
	return cand.ImageURL, cand.ImageURL, cand.Album, cand.Year, decision, plogs, qSent, nil
}

func (s *EnrichmentService) FindCover(ctx context.Context, title, artist, album string) (coverURL, thumbURL, foundAlbum string, year int, err error) {
	c, th, alb, y, _, _, _, err := s.FindCoverWithDecision(ctx, title, artist, album)
	return c, th, alb, y, err
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
