package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"musix/engine/internal/database"
	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
	"musix/engine/internal/services"
	"musix/engine/internal/stream"
	"musix/engine/internal/telegram"
)

// Server is the local loopback HTTP REST and audio streaming server.
type Server struct {
	listener   net.Listener
	port       int
	token      string
	httpSrv    *http.Server
	streamer   *stream.Streamer
	client     telegram.Client
	db         *database.DB
	chatID     int64
	artworkDir string
	enrichment *services.EnrichmentService
	catchup    *telegram.CatchupService
	mu         sync.Mutex
	started    time.Time
}

// Config specifies server configuration options.
type Config struct {
	Token       string
	Port        int
	Client      telegram.Client
	DB          *database.DB
	ChatID      int64
	ArtworkDir  string
	Enrichment  *services.EnrichmentService
	Catchup     *telegram.CatchupService
	Diagnostics *stream.StreamDiagnostics
}

// New creates and configures a new loopback server bound to 127.0.0.1:<port>.
func New(cfg Config) (*Server, error) {
	token := cfg.Token
	if token == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("failed to generate random token: %w", err)
		}
		token = hex.EncodeToString(b)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to bind loopback listener: %w", err)
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return nil, fmt.Errorf("unexpected listener address type: %T", listener.Addr())
	}

	normChatID, _ := database.NormalizeChatID(cfg.ChatID)

	s := &Server{
		listener:   listener,
		port:       tcpAddr.Port,
		token:      token,
		streamer:   stream.NewStreamer(cfg.Client),
		client:     cfg.Client,
		db:         cfg.DB,
		chatID:     normChatID,
		artworkDir: cfg.ArtworkDir,
		enrichment: cfg.Enrichment,
		catchup:    cfg.Catchup,
		started:    time.Now(),
	}
	if cfg.Diagnostics != nil {
		s.streamer.SetDiagnostics(cfg.Diagnostics)
	}

	mux := http.NewServeMux()

	// Unauthenticated health endpoint
	mux.HandleFunc("/health", s.handleHealth)

	// Stream and Media endpoints (authenticated)
	mux.HandleFunc("/stream/", s.authMiddleware(s.handleStream))
	mux.HandleFunc("/artwork/", s.authMiddleware(s.handleArtwork))
	mux.HandleFunc("/lyrics/", s.authMiddleware(s.handleLyrics))

	// REST API Catalog Navigation endpoints (authenticated)
	mux.HandleFunc("/api/topics", s.authMiddleware(s.handleTopics))
	mux.HandleFunc("/api/topics/", s.authMiddleware(s.handleTopicTracks))
	mux.HandleFunc("/api/artists", s.authMiddleware(s.handleArtists))
	mux.HandleFunc("/api/artists/", s.authMiddleware(s.handleArtistDetail))
	mux.HandleFunc("/api/albums", s.authMiddleware(s.handleAlbums))
	mux.HandleFunc("/api/albums/", s.authMiddleware(s.handleAlbumDetail))
	mux.HandleFunc("/api/tracks", s.authMiddleware(s.handleTracks))
	mux.HandleFunc("/api/search", s.authMiddleware(s.handleSearch))
	mux.HandleFunc("/api/daily_mix", s.authMiddleware(s.handleDailyMix))
	mux.HandleFunc("/api/rescan", s.authMiddleware(s.handleRescan))

	// StreamX Extension Adapter endpoints (authenticated)
	s.registerStreamXRoutes(mux)

	s.httpSrv = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // Streaming needs open write timeout
		IdleTimeout:  60 * time.Second,
	}

	return s, nil
}

// Start runs the HTTP server in a background goroutine.
func (s *Server) Start() {
	go func() {
		_ = s.httpSrv.Serve(s.listener)
	}()
}

// Close shuts down the server and listener.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpSrv != nil {
		return s.httpSrv.Close()
	}
	return nil
}

// Port returns the bound port.
func (s *Server) Port() int {
	return s.port
}

// Token returns the session security token.
func (s *Server) Token() string {
	return s.token
}

// Streamer returns the underlying audio streamer.
func (s *Server) Streamer() *stream.Streamer {
	return s.streamer
}


func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}
		if token == "" {
			token = r.Header.Get("X-Engine-Token")
		}

		if token == "" || token != s.token {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"ok":     false,
				"error":  "Unauthorized",
				"status": http.StatusUnauthorized,
			})
			return
		}

		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"status": "running",
		"port":   s.port,
		"uptime": time.Since(s.started).String(),
		"ready":  s.client != nil && s.client.IsReady(),
	})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Track ID required", http.StatusBadRequest)
		return
	}
	trackID := parts[0]
	s.streamer.ServeStream(w, r, trackID)
}

// SetEnrichment associates the enrichment service with this server.
func (s *Server) SetEnrichment(enrichment *services.EnrichmentService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrichment = enrichment
}

// SetCatchup associates the catchup service with this server.
func (s *Server) SetCatchup(catchup *telegram.CatchupService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchup = catchup
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	catchup := s.catchup
	s.mu.Unlock()

	if catchup == nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"ok":    false,
			"error": "Catchup service not available",
		})
		return
	}
	go func() {
		_ = catchup.RescanFromScratch(context.Background(), s.chatID)
	}()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "Full database rescan initiated from scratch",
	})
}

func (s *Server) handleArtwork(w http.ResponseWriter, r *http.Request) {
	trackID := strings.TrimPrefix(r.URL.Path, "/artwork/")
	if trackID == "" {
		http.Error(w, "Track ID required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// 1. Check local cached artwork on disk
	if s.artworkDir != "" {
		localPath := filepath.Join(s.artworkDir, trackID+".jpg")
		if info, err := os.Stat(localPath); err == nil && info.Size() > 0 {
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, localPath)
			return
		}
	}

	// 2. On-demand embedded metadata & picture extraction from first chunk
	if s.streamer != nil {
		chunk, err := s.streamer.ReadChunk(ctx, trackID, 0, 512*1024)
		if err == nil && len(chunk) > 0 {
			meta := metadata.ParseAudioHeader(chunk)
			if meta != nil && len(meta.Picture) > 0 {
				if s.artworkDir != "" {
					localPath := filepath.Join(s.artworkDir, trackID+".jpg")
					_ = os.WriteFile(localPath, meta.Picture, 0644)
				}
				if s.db != nil {
					_ = s.db.UpdateTrackEnrichmentWithAlbum(ctx, trackID, meta.Album, "/artwork/"+trackID, "/artwork/"+trackID, "", "", meta.Year)
				}
				mime := meta.PictureMime
				if mime == "" {
					mime = "image/jpeg"
				}
				w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
				w.Header().Set("Content-Type", mime)
				_, _ = w.Write(meta.Picture)
				return
			}
		}
	}

	// 3. Check if track has remote CoverURL or ThumbURL
	var coverURL string
	var tr *models.Track
	if s.db != nil {
		if t, err := s.db.GetTrack(ctx, trackID); err == nil && t != nil {
			tr = t
			if strings.HasPrefix(t.CoverURL, "http://") || strings.HasPrefix(t.CoverURL, "https://") {
				coverURL = t.CoverURL
			} else if strings.HasPrefix(t.ThumbURL, "http://") || strings.HasPrefix(t.ThumbURL, "https://") {
				coverURL = t.ThumbURL
			}
		}
	}

	if coverURL == "" && s.client != nil {
		if t, err := s.client.ResolveTrack(ctx, trackID); err == nil && t != nil {
			if strings.HasPrefix(t.CoverURL, "http://") || strings.HasPrefix(t.CoverURL, "https://") {
				coverURL = t.CoverURL
			}
		}
	}

	if coverURL != "" {
		http.Redirect(w, r, coverURL, http.StatusFound)
		return
	}

	// 4. On-demand fallback search with iTunes (including country=IN)
	if s.enrichment != nil && tr != nil {
		title := metadata.StripNoise(tr.Title)
		artist := metadata.StripNoise(tr.Artist)
		album := metadata.StripNoise(tr.Album)
		if c, th, alb, y, err := s.enrichment.FindCover(ctx, title, artist, album); err == nil && c != "" {
			_ = s.db.UpdateTrackEnrichmentWithAlbum(ctx, trackID, alb, c, th, "", "", y)
			http.Redirect(w, r, c, http.StatusFound)
			return
		}
	}

	http.Error(w, "Artwork not found", http.StatusNotFound)
}

func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	trackID := strings.TrimPrefix(r.URL.Path, "/lyrics/")
	if trackID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":     false,
			"error":  "Track ID required",
			"status": http.StatusBadRequest,
		})
		return
	}

	ctx := r.Context()
	var plainLyrics, syncedLyrics string

	if s.db != nil {
		if tr, err := s.db.GetTrack(ctx, trackID); err == nil && tr != nil {
			plainLyrics = tr.Lyrics
			syncedLyrics = tr.SyncedLyrics
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"track_id":      trackID,
		"plain_lyrics":  plainLyrics,
		"synced_lyrics": syncedLyrics,
	})
}

func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "items": []interface{}{}})
		return
	}

	topics, err := s.db.ListTopics(r.Context(), s.chatID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"items": topics,
	})
}

func (s *Server) handleTopicTracks(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "items": []interface{}{}})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/topics/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "Topic ID required"})
		return
	}

	topicID64, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "Invalid Topic ID"})
		return
	}

	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)

	tracks, err := s.db.ListTracksByTopic(r.Context(), s.chatID, int32(topicID64), offset, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"topic_id": topicID64,
		"items":    tracks,
	})
}

func (s *Server) handleArtists(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "total": 0, "items": []interface{}{}})
		return
	}

	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)

	artists, total, err := s.db.ListArtists(r.Context(), offset, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"total": total,
		"items": artists,
	})
}

func (s *Server) handleArtistDetail(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{"ok": false, "error": "Database not initialized"})
		return
	}

	artistName := strings.TrimPrefix(r.URL.Path, "/api/artists/")
	artistName = strings.TrimPrefix(artistName, "artist_")
	limit := parseQueryInt(r, "limit", 50)

	tracks, err := s.db.GetArtistTracks(r.Context(), artistName, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"artist": artistName,
		"tracks": tracks,
	})
}

func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "total": 0, "items": []interface{}{}})
		return
	}

	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)

	albums, total, err := s.db.ListAlbums(r.Context(), offset, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"total": total,
		"items": albums,
	})
}

func (s *Server) handleAlbumDetail(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{"ok": false, "error": "Database not initialized"})
		return
	}

	albumTitle := strings.TrimPrefix(r.URL.Path, "/api/albums/")
	albumTitle = strings.TrimPrefix(albumTitle, "album_")
	limit := parseQueryInt(r, "limit", 50)

	tracks, err := s.db.GetAlbumTracks(r.Context(), albumTitle, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"album":  albumTitle,
		"tracks": tracks,
	})
}

func (s *Server) handleTracks(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "total": 0, "items": []interface{}{}})
		return
	}

	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)

	tracks, total, err := s.db.ListTracks(r.Context(), offset, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"total": total,
		"items": tracks,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "items": []interface{}{}})
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "query": "", "items": []interface{}{}})
		return
	}

	limit := parseQueryInt(r, "limit", 30)
	tracks, err := s.db.SearchTracks(r.Context(), query, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"query": query,
		"items": tracks,
	})
}

func (s *Server) handleDailyMix(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "items": []interface{}{}})
		return
	}

	limit := parseQueryInt(r, "limit", 25)
	tracks, err := s.db.GetRandomTracks(r.Context(), limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"title": "Daily Mix",
		"items": tracks,
	})
}

func parseQueryInt(r *http.Request, key string, defaultVal int) int {
	val := strings.TrimSpace(r.URL.Query().Get(key))
	if val == "" {
		return defaultVal
	}
	if n, err := strconv.Atoi(val); err == nil && n >= 0 {
		return n
	}
	return defaultVal
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
