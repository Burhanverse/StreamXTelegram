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
	"strconv"
	"strings"
	"sync"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/logger"
	"musix/engine/internal/metadata"
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

// EnrichmentService manages asynchronous metadata enrichment (artwork, lyrics, release year).
type EnrichmentService struct {
	db         *database.DB
	reader     ChunkReader
	artworkDir string
	httpClient *http.Client
	queue      chan string
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.RWMutex
	coverCache sync.Map
}

// NewEnrichmentService creates a new EnrichmentService and starts its background workers.
func NewEnrichmentService(db *database.DB) *EnrichmentService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &EnrichmentService{
		db: db,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		queue:  make(chan string, 1000),
		ctx:    ctx,
		cancel: cancel,
	}

	// Run 3 concurrent enrichment workers for fast batch processing
	for i := 0; i < 3; i++ {
		s.wg.Add(1)
		go s.workerLoop()
	}

	return s
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

// Stop gracefully stops the background enrichment queue.
func (s *EnrichmentService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Enqueue adds a track ID to the background enrichment queue.
func (s *EnrichmentService) Enqueue(trackID string) {
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
		case trackID := <-s.queue:
			_ = s.EnrichTrack(s.ctx, trackID)
			// Small pacing between requests to avoid burst rate-limits
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(80 * time.Millisecond):
			}
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

	var year int = track.Year
	album := track.Album
	// Intelligently extract clean title and artist from tags and Telegram filename
	title, artist := metadata.ExtractTitleAndArtist(track.Title, track.Artist, track.FileName)

	var localArt string
	if artworkDir != "" {
		localArt = filepath.Join(artworkDir, trackID+".jpg")
	}

	// 1. Try embedded metadata extraction if artist or album is generic/unknown
	if reader != nil && (artist == "" || artist == "Unknown Artist" || album == "" || album == "Telegram Music" || strings.HasPrefix(title, "Track ")) {
		readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
		chunk, err := reader.ReadChunk(readCtx, trackID, 0, 512*1024)
		readCancel()
		if err == nil && len(chunk) > 0 {
			meta := metadata.ParseAudioHeader(chunk)
			if meta != nil {
				// Embedded Picture found -> write to local cache for offline/backup
				if len(meta.Picture) > 0 && localArt != "" {
					_ = os.WriteFile(localArt, meta.Picture, 0644)
					logEnrich.Infof("Extracted and cached embedded cover art (%d bytes) for track %s", len(meta.Picture), trackID)
				}
				// Embedded Title if cleaner
				if meta.Title != "" && len(meta.Title) > 1 {
					cleanT := metadata.StripNoise(meta.Title)
					if cleanT != "" {
						title = cleanT
					}
				}
				// Embedded Artist if currently unknown/default
				if meta.Artist != "" && (artist == "" || artist == "Unknown Artist") {
					cleanA := metadata.StripNoise(meta.Artist)
					if cleanA != "" {
						artist = cleanA
					}
				}
				// Embedded Album name
				if meta.Album != "" && (album == "" || album == "Telegram Music") {
					album = meta.Album
				}
				// Embedded Year
				if meta.Year > 0 && year == 0 {
					year = meta.Year
				}
			}
		}
	}

	cleanTitle := metadata.StripNoise(title)
	cleanArtist := metadata.StripNoise(artist)
	cleanAlbum := metadata.StripNoise(album)

	// 2. Fetch official, unique, high-resolution HTTPS cover art from JioSaavn / iTunes / Deezer
	// StreamX strictly requires HTTPS thumbnail URLs (takeIf { it.startsWith("https://") })
	var coverURL, thumbURL string
	if strings.HasPrefix(track.CoverURL, "https://") {
		coverURL = track.CoverURL
		thumbURL = track.ThumbURL
	} else {
		// Check in-memory cache first
		cacheKey := strings.ToLower(fmt.Sprintf("%s - %s", cleanArtist, cleanTitle))
		if val, ok := s.coverCache.Load(cacheKey); ok {
			coverURL = val.(string)
			thumbURL = coverURL
		} else {
			c, th, discoveredAlbum, y, err := s.FindCover(ctx, cleanTitle, cleanArtist, cleanAlbum)
			if err == nil && c != "" {
				coverURL = c
				thumbURL = th
				s.coverCache.Store(cacheKey, coverURL)
				if year == 0 && y > 0 {
					year = y
				}
				if (album == "" || album == "Telegram Music") && discoveredAlbum != "" {
					album = discoveredAlbum
				}
			}
		}
	}

	// 3. Lyrics retrieval
	var plainLyrics, syncedLyrics string
	if track.Lyrics == "" {
		res, err := s.FindLyrics(ctx, cleanTitle, cleanArtist)
		if err == nil && res != nil {
			plainLyrics = res.PlainLyrics
			syncedLyrics = res.SyncedLyrics
		}
	}

	// 4. Persist enrichment in DB with clean Title, Artist, Album, and real https:// CoverURL
	if coverURL != "" || plainLyrics != "" || year > 0 || album != track.Album || title != track.Title || artist != track.Artist {
		if err := s.db.UpdateTrackFullMetadata(ctx, trackID, title, artist, album, coverURL, thumbURL, plainLyrics, syncedLyrics, year); err != nil {
			logEnrich.Warnf("Failed to persist enrichment for %s: %v", trackID, err)
			return err
		}
		logEnrich.Infof("Successfully enriched track %s: %s - %s (album: %s, cover: %s)", trackID, artist, title, album, coverURL)
	}

	return nil
}

// FindCover searches JioSaavn (primary for Indian music), iTunes (secondary with 1000x1000 upgrade), and Deezer.
// Strictly queries by track title + artist name to return track coverart, never falling back to album thumbnail.
func (s *EnrichmentService) FindCover(ctx context.Context, title, artist, album string) (coverURL, thumbURL, foundAlbum string, year int, err error) {
	// 1. Try JioSaavn first (the gold standard for Bollywood, Indian pop & Indie music)
	if c, th, alb, y, err := s.searchJioSaavn(ctx, title, artist); err == nil && c != "" {
		return c, th, alb, y, nil
	}

	// 2. Try iTunes Search API (Storefront IN first, then worldwide fallback, 1000x1000 upgrade)
	if c, th, y, err := s.searchITunes(ctx, title, artist); err == nil && c != "" {
		return c, th, album, y, nil
	}

	// 3. Try Deezer search
	if c, th, err := s.searchDeezer(ctx, title, artist); err == nil && c != "" {
		return c, th, album, 0, nil
	}

	return "", "", "", 0, fmt.Errorf("no cover found for %s - %s", artist, title)
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

type jioSaavnArtistItem struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type jioSaavnResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Type     string `json:"type"`
	Image    string `json:"image"`
	Year     string `json:"year"`
	Album    string `json:"album"`
	Singers  string `json:"singers"`
	MoreInfo struct {
		Album     string                `json:"album"`
		Music     string                `json:"music"`
		ArtistMap struct {
			PrimaryArtists  []jioSaavnArtistItem `json:"primary_artists"`
			FeaturedArtists []jioSaavnArtistItem `json:"featured_artists"`
			Artists         []jioSaavnArtistItem `json:"artists"`
		} `json:"artistMap"`
	} `json:"more_info"`
}

func (r *jioSaavnResult) matchesArtist(trackArtists []string) bool {
	if len(trackArtists) == 0 {
		return true
	}
	var sb strings.Builder
	if r.Subtitle != "" {
		sub := html.UnescapeString(r.Subtitle)
		if idx := strings.Index(sub, " - "); idx > 0 {
			sb.WriteString(strings.ToLower(sub[:idx]))
		} else {
			sb.WriteString(strings.ToLower(sub))
		}
		sb.WriteString(" ")
	}
	if r.Singers != "" {
		sb.WriteString(strings.ToLower(html.UnescapeString(r.Singers)))
		sb.WriteString(" ")
	}
	if r.MoreInfo.Music != "" {
		sb.WriteString(strings.ToLower(html.UnescapeString(r.MoreInfo.Music)))
		sb.WriteString(" ")
	}
	for _, a := range r.MoreInfo.ArtistMap.PrimaryArtists {
		sb.WriteString(strings.ToLower(html.UnescapeString(a.Name)))
		sb.WriteString(" ")
	}
	for _, a := range r.MoreInfo.ArtistMap.Artists {
		sb.WriteString(strings.ToLower(html.UnescapeString(a.Name)))
		sb.WriteString(" ")
	}
	combined := sb.String()

	for _, ta := range trackArtists {
		lowerTA := strings.ToLower(ta)
		if len(lowerTA) >= 2 && strings.Contains(combined, lowerTA) {
			return true
		}
	}
	return false
}

type jioSaavnResponse struct {
	Total   int              `json:"total"`
	Results []jioSaavnResult `json:"results"`
}

// searchJioSaavn queries the official JioSaavn API (ported from sumitkolhe/jiosaavn-api core endpoints).
func (s *EnrichmentService) searchJioSaavn(ctx context.Context, title, artist string) (coverURL, thumbURL, album string, year int, err error) {
	trackArtists := splitArtists(artist)
	cleanT := metadata.StripNoise(title)
	coreT := coreSongTitle(cleanT)

	queries := make([]string, 0, 4)
	if len(trackArtists) > 0 {
		// Strict queries: Always search with track name + artist name(s)
		queries = append(queries, fmt.Sprintf("%s %s", cleanT, trackArtists[0]))
		queries = append(queries, fmt.Sprintf("%s %s", trackArtists[0], cleanT))
		if len(trackArtists) > 1 {
			queries = append(queries, fmt.Sprintf("%s %s", cleanT, trackArtists[1]))
		}
		if coreT != "" && coreT != cleanT {
			queries = append(queries, fmt.Sprintf("%s %s", coreT, trackArtists[0]))
		}
	} else {
		// Only fallback to title alone if artist is completely unknown
		queries = append(queries, cleanT)
		if coreT != "" && coreT != cleanT {
			queries = append(queries, coreT)
		}
	}

	for _, q := range queries {
		apiURL := fmt.Sprintf("https://www.jiosaavn.com/api.php?__call=search.getResults&_format=json&_marker=0&api_version=4&ctx=web6dot0&q=%s&p=1&n=5", url.QueryEscape(q))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
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

		lowerCleanT := strings.ToLower(cleanT)
		lowerCoreT := strings.ToLower(coreT)

		var matched *jioSaavnResult
		// 1. First pass: exact or core title match + STRICT artist verification + song type only
		for i := range payload.Results {
			r := &payload.Results[i]
			if r.Type != "" && r.Type != "song" {
				continue // Must be track/song coverart, never an album thumbnail
			}
			if len(trackArtists) > 0 && !r.matchesArtist(trackArtists) {
				continue
			}
			rTitle := strings.ToLower(strings.TrimSpace(html.UnescapeString(r.Title)))
			if rTitle == lowerCleanT || (lowerCoreT != "" && rTitle == lowerCoreT) {
				matched = r
				break
			}
		}

		// 2. Second pass: partial title match + STRICT artist verification + song type only
		if matched == nil {
			for i := range payload.Results {
				r := &payload.Results[i]
				if r.Type != "" && r.Type != "song" {
					continue // Must be track/song coverart, never an album thumbnail
				}
				if len(trackArtists) > 0 && !r.matchesArtist(trackArtists) {
					continue
				}
				rTitle := strings.ToLower(strings.TrimSpace(html.UnescapeString(r.Title)))
				if !strings.Contains(lowerCleanT, "remix") && strings.Contains(rTitle, "remix") {
					continue
				}
				if len(rTitle) > 3 && (strings.Contains(rTitle, lowerCleanT) || strings.Contains(lowerCleanT, rTitle) ||
					(lowerCoreT != "" && (strings.Contains(rTitle, lowerCoreT) || strings.Contains(lowerCoreT, rTitle)))) {
					matched = r
					break
				}
			}
		}

		if matched != nil && matched.Image != "" {
			rawImg := strings.TrimSpace(matched.Image)
			highRes := strings.Replace(rawImg, "150x150", "500x500", 1)
			highRes = strings.Replace(highRes, "50x50", "500x500", 1)
			if strings.HasPrefix(highRes, "http://") {
				highRes = "https://" + strings.TrimPrefix(highRes, "http://")
			}
			thumb := strings.Replace(highRes, "500x500", "150x150", 1)

			alb := strings.TrimSpace(html.UnescapeString(matched.MoreInfo.Album))
			if alb == "" {
				alb = strings.TrimSpace(html.UnescapeString(matched.Album))
			}

			y := 0
			if len(matched.Year) >= 4 {
				y, _ = strconv.Atoi(matched.Year[:4])
			}

			if strings.HasPrefix(highRes, "https://") {
				return highRes, thumb, alb, y, nil
			}
		}
	}

	return "", "", "", 0, fmt.Errorf("jiosaavn: no cover found for %s - %s", artist, title)
}

func (s *EnrichmentService) searchITunes(ctx context.Context, title, artist string) (coverURL, thumbURL string, year int, err error) {
	trackArtists := splitArtists(artist)
	cleanT := metadata.StripNoise(title)
	storefronts := []string{"IN", ""}
	queries := make([]string, 0, 4)

	if len(trackArtists) > 0 {
		queries = append(queries, fmt.Sprintf("%s %s", cleanT, trackArtists[0]))
		queries = append(queries, fmt.Sprintf("%s %s", trackArtists[0], cleanT))
		if len(trackArtists) > 1 {
			queries = append(queries, fmt.Sprintf("%s %s", cleanT, trackArtists[1]))
		}
	} else if cleanT != "" {
		queries = append(queries, cleanT)
	}

	for _, q := range queries {
		for _, sf := range storefronts {
			apiURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=song&limit=5", url.QueryEscape(q))
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
				resp.Body.Close()
				return "", "", 0, fmt.Errorf("itunes 429")
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				continue
			}

			var payload struct {
				ResultCount int `json:"resultCount"`
				Results     []struct {
					TrackName     string `json:"trackName"`
					ArtistName    string `json:"artistName"`
					ArtworkUrl100 string `json:"artworkUrl100"`
					ArtworkUrl60  string `json:"artworkUrl60"`
					ReleaseDate   string `json:"releaseDate"`
				} `json:"results"`
			}

			decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if decodeErr != nil {
				continue
			}

			if payload.ResultCount > 0 && len(payload.Results) > 0 {
				var chosen struct {
					ArtworkUrl100 string
					ArtworkUrl60  string
					ReleaseDate   string
				}
				foundMatch := false
				lowerTitle := strings.ToLower(cleanT)
				for _, r := range payload.Results {
					lowerTrack := strings.ToLower(r.TrackName)
					lowerArtist := strings.ToLower(r.ArtistName)

					// Strict artist verification
					if len(trackArtists) > 0 {
						matchedArt := false
						for _, ta := range trackArtists {
							if strings.Contains(lowerArtist, strings.ToLower(ta)) || strings.Contains(strings.ToLower(ta), lowerArtist) {
								matchedArt = true
								break
							}
						}
						if !matchedArt {
							continue // Skip: artist mismatch
						}
					}

					// Title verification
					if strings.Contains(lowerTrack, lowerTitle) || strings.Contains(lowerTitle, lowerTrack) {
						chosen.ArtworkUrl100 = r.ArtworkUrl100
						chosen.ArtworkUrl60 = r.ArtworkUrl60
						chosen.ReleaseDate = r.ReleaseDate
						foundMatch = true
						break
					}
				}
				if foundMatch && chosen.ArtworkUrl100 != "" {
					highRes := strings.Replace(chosen.ArtworkUrl100, "100x100bb.jpg", "1000x1000bb.jpg", 1)
					y := 0
					if len(chosen.ReleaseDate) >= 4 {
						y, _ = strconv.Atoi(chosen.ReleaseDate[:4])
					}
					return highRes, chosen.ArtworkUrl60, y, nil
				}
			}
		}
	}

	return "", "", 0, fmt.Errorf("itunes: no cover found for %s - %s", artist, title)
}

func (s *EnrichmentService) searchDeezer(ctx context.Context, title, artist string) (coverURL, thumbURL string, err error) {
	query := fmt.Sprintf(`artist:"%s" track:"%s"`, artist, title)
	apiURL := fmt.Sprintf("https://api.deezer.com/search?q=%s&limit=3", url.QueryEscape(query))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("deezer status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			Album struct {
				CoverBig    string `json:"cover_big"`
				CoverMedium string `json:"cover_medium"`
			} `json:"album"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}

	if len(payload.Data) > 0 {
		return payload.Data[0].Album.CoverBig, payload.Data[0].Album.CoverMedium, nil
	}

	return "", "", nil
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
