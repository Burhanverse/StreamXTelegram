package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
)

// registerStreamXRoutes mounts the StreamX extension JSON adapter endpoints.
func (s *Server) registerStreamXRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/streamx/health", s.authMiddleware(s.handleStreamXHealth))
	mux.HandleFunc("/api/streamx/account", s.authMiddleware(s.handleStreamXAccount))
	mux.HandleFunc("/api/streamx/home", s.authMiddleware(s.handleStreamXHome))
	mux.HandleFunc("/api/streamx/browse", s.authMiddleware(s.handleStreamXBrowse))
	mux.HandleFunc("/api/streamx/library", s.authMiddleware(s.handleStreamXLibrary))
	mux.HandleFunc("/api/streamx/search", s.authMiddleware(s.handleStreamXSearch))
	mux.HandleFunc("/api/streamx/detail", s.authMiddleware(s.handleStreamXDetail))
	mux.HandleFunc("/api/streamx/stream", s.authMiddleware(s.handleStreamXStream))
	mux.HandleFunc("/api/streamx/saved", s.authMiddleware(s.handleStreamXSaved))
	mux.HandleFunc("/api/streamx/save", s.authMiddleware(s.handleStreamXSave))
	mux.HandleFunc("/api/streamx/lyrics", s.authMiddleware(s.handleStreamXLyrics))
	mux.HandleFunc("/api/streamx/playlists", s.authMiddleware(s.handleStreamXPlaylists))
	mux.HandleFunc("/api/streamx/create_playlist", s.authMiddleware(s.handleStreamXCreatePlaylist))
	mux.HandleFunc("/api/streamx/add_to_playlist", s.authMiddleware(s.handleStreamXAddToPlaylist))
	mux.HandleFunc("/api/streamx/remove_from_playlist", s.authMiddleware(s.handleStreamXRemoveFromPlaylist))
}

var (
	topicParenRegex  = regexp.MustCompile(`(?i)\s*\(\s*topic\s*\d*\s*\)`)
	topicPrefixRegex = regexp.MustCompile(`(?i)^\s*topic\s*(\d+)\s*$`)
)

func formatTopicPlaylistTitle(name string, topicID int32) string {
	name = strings.TrimSpace(name)
	name = topicParenRegex.ReplaceAllString(name, "")
	name = strings.TrimSpace(name)
	if matches := topicPrefixRegex.FindStringSubmatch(name); len(matches) > 1 {
		return matches[1]
	}
	if name == "" {
		return strconv.Itoa(int(topicID))
	}
	return name
}

func formatTopicSubtitle(trackCount int) string {
	if trackCount == 1 {
		return "1 track"
	}
	return fmt.Sprintf("%d tracks", trackCount)
}

func (s *Server) formatThumbnailURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "content://") {
		return rawURL
	}
	return ""
}

func (s *Server) trackArtworkURL(tr *models.Track) string {
	if tr == nil {
		return ""
	}
	// Source order: (a) embedded art from local file served through content:// provider first
	if s.artworkDir != "" {
		artFile := filepath.Join(s.artworkDir, tr.ID+".jpg")
		if fi, err := os.Stat(artFile); err == nil && fi.Size() > 0 {
			return fmt.Sprintf("content://com.music.streamx.extensions.telegram.source/art/%s", tr.ID)
		}
	}
	thumbURL := strings.TrimSpace(tr.CoverURL)
	if thumbURL == "" && tr.ThumbURL != "" {
		thumbURL = strings.TrimSpace(tr.ThumbURL)
	}
	if strings.HasPrefix(thumbURL, "https://") || strings.HasPrefix(thumbURL, "content://") {
		return thumbURL
	}
	if s.enrichment != nil {
		if cached := s.enrichment.GetCachedCover(tr.Title, tr.Artist); cached != "" {
			return cached
		}
	}
	return ""
}

func (s *Server) topicArtworkURL(ctx context.Context, topicID int32, explicitArt string) string {
	explicitArt = strings.TrimSpace(explicitArt)
	if strings.HasPrefix(explicitArt, "https://") || strings.HasPrefix(explicitArt, "content://") {
		return explicitArt
	}
	if s.db != nil {
		tracks, err := s.db.ListTracksByTopic(ctx, s.chatID, topicID, 0, 10)
		if err == nil {
			for _, tr := range tracks {
				if art := s.trackArtworkURL(tr); art != "" {
					return art
				}
			}
		}
	}
	return ""
}

func (s *Server) playlistArtworkURL(ctx context.Context, plID string, explicitArt string) string {
	explicitArt = strings.TrimSpace(explicitArt)
	if strings.HasPrefix(explicitArt, "https://") || strings.HasPrefix(explicitArt, "content://") {
		return explicitArt
	}
	if s.db != nil {
		tracks, err := s.db.GetPlaylistTracks(ctx, plID, 0, 10)
		if err == nil {
			for _, tr := range tracks {
				if art := s.trackArtworkURL(tr); art != "" {
					return art
				}
			}
		}
	}
	return ""
}

func (s *Server) albumArtworkURL(ctx context.Context, albumTitle string, explicitCover string) string {
	explicitCover = strings.TrimSpace(explicitCover)
	if strings.HasPrefix(explicitCover, "https://") || strings.HasPrefix(explicitCover, "content://") {
		return explicitCover
	}
	if s.db != nil {
		tracks, err := s.db.GetAlbumTracks(ctx, albumTitle, 50)
		if err == nil && len(tracks) > 0 {
			// Consensus cover: most common cover among its tracks
			counts := make(map[string]int)
			for _, tr := range tracks {
				if art := s.trackArtworkURL(tr); art != "" {
					counts[art]++
				}
			}
			var bestArt string
			var maxCount int
			for art, count := range counts {
				if count > maxCount {
					maxCount = count
					bestArt = art
				}
			}
			if bestArt != "" {
				return bestArt
			}
		}
	}
	return ""
}

func (s *Server) artistArtworkURL(ctx context.Context, artistName string, explicitThumb string) string {
	explicitThumb = strings.TrimSpace(explicitThumb)
	if strings.HasPrefix(explicitThumb, "https://") || strings.HasPrefix(explicitThumb, "content://") {
		return explicitThumb
	}
	// Do not reuse track cover art as an artist photo (Prompt 4 requirement)
	return ""
}

func (s *Server) handleStreamXHealth(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "Connected as Telegram Engine",
		"capabilities": []string{
			"home", "browse", "library", "search", "detail", "account", "stream", "saved", "save", "lyrics", "playlists", "create_playlist", "add_to_playlist", "remove_from_playlist",
		},
	})
}


func (s *Server) handleStreamXAccount(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":            "telegram:local",
		"name":          "Telegram Library",
		"thumbnail_url": "",
	})
}

func (s *Server) handleStreamXHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sections := make([]map[string]interface{}, 0)

	account := map[string]interface{}{
		"id":            "telegram:local",
		"name":          "Telegram Library",
		"thumbnail_url": "",
	}

	if s.db == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"account":  account,
			"sections": sections,
		})
		return
	}

	// 1. Daily Mix section
	if daily, err := s.db.GetRandomTracks(ctx, 15); err == nil && len(daily) > 0 {
		items := make([]map[string]interface{}, 0, len(daily))
		for _, tr := range daily {
			items = append(items, s.trackToStreamXItem(tr))
		}
		sections = append(sections, map[string]interface{}{
			"id":       "home:daily",
			"title":    "Daily Mix",
			"subtitle": "Fresh picks from your Telegram library",
			"items":    items,
		})
	}

	// 2. Topics / Playlists section
	if topics, err := s.db.ListTopics(ctx, s.chatID); err == nil && len(topics) > 0 {
		items := make([]map[string]interface{}, 0, len(topics))
		for _, top := range topics {
			items = append(items, map[string]interface{}{
				"type":          "playlist",
				"id":            strconv.Itoa(int(top.ID)),
				"title":         formatTopicPlaylistTitle(top.Name, top.ID),
				"subtitle":      formatTopicSubtitle(top.TrackCount),
				"thumbnail_url": s.topicArtworkURL(ctx, top.ID, top.ArtworkURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "home:topics",
			"title":    "Playlists",
			"subtitle": "Playlists in your Telegram library",
			"items":    items,
		})
	}

	// 3. Recent Tracks section
	if recent, _, err := s.db.ListTracks(ctx, 0, 15); err == nil && len(recent) > 0 {
		items := make([]map[string]interface{}, 0, len(recent))
		for _, tr := range recent {
			items = append(items, s.trackToStreamXItem(tr))
		}
		sections = append(sections, map[string]interface{}{
			"id":       "home:recent",
			"title":    "Recently Added",
			"subtitle": "Latest tracks synchronized",
			"items":    items,
		})
	}

	budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
		"account":  account,
		"sections": sections,
	})
}

func (s *Server) handleStreamXBrowse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sections := make([]map[string]interface{}, 0)

	account := map[string]interface{}{
		"id":            "telegram:local",
		"name":          "Telegram Library",
		"thumbnail_url": "",
	}

	if s.db == nil {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"account":  account,
			"sections": sections,
		})
		return
	}


	// 1. Artists section
	if artists, _, err := s.db.ListArtists(ctx, 0, 30); err == nil && len(artists) > 0 {
		items := make([]map[string]interface{}, 0, len(artists))
		for _, art := range artists {
			items = append(items, map[string]interface{}{
				"type":          "artist",
				"id":            art.ID,
				"title":         art.Name,
				"subtitle":      fmt.Sprintf("%d tracks", art.TrackCount),
				"thumbnail_url": s.artistArtworkURL(ctx, art.Name, art.ThumbURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "browse:artists",
			"title":    "Artists",
			"subtitle": "Explore by artist",
			"items":    items,
		})
	}

	// 2. Albums section
	if albums, _, err := s.db.ListAlbums(ctx, 0, 30); err == nil && len(albums) > 0 {
		items := make([]map[string]interface{}, 0, len(albums))
		for _, alb := range albums {
			items = append(items, map[string]interface{}{
				"type":          "album",
				"id":            alb.ID,
				"title":         alb.Title,
				"subtitle":      alb.Artist,
				"thumbnail_url": s.albumArtworkURL(ctx, alb.Title, alb.CoverURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "browse:albums",
			"title":    "Albums",
			"subtitle": "Explore by album",
			"items":    items,
		})
	}

	budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
		"account":  account,
		"sections": sections,
	})
}

func (s *Server) handleStreamXLibrary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sections := make([]map[string]interface{}, 0)

	account := map[string]interface{}{
		"id":            "telegram:local",
		"name":          "Telegram Library",
		"thumbnail_url": "",
	}

	if s.db == nil {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"account":  account,
			"sections": sections,
		})
		return
	}

	// 1. Liked / All Songs section (consumed by StreamX ApkCatalogue to populate LibraryPage.likedSongs & librarySongs)
	if allTracks, _, err := s.db.ListTracks(ctx, 0, 50); err == nil && len(allTracks) > 0 {
		trackItems := make([]map[string]interface{}, 0, len(allTracks))
		for _, tr := range allTracks {
			trackItems = append(trackItems, s.trackToStreamXItem(tr))
		}
		sections = append(sections, map[string]interface{}{
			"id":       "liked",
			"title":    "All Tracks",
			"subtitle": fmt.Sprintf("%d tracks in Telegram library", len(allTracks)),
			"items":    trackItems,
		})
	}

	// 2. Playlists
	if topics, err := s.db.ListTopics(ctx, s.chatID); err == nil && len(topics) > 0 {
		items := make([]map[string]interface{}, 0, len(topics))
		for _, top := range topics {
			items = append(items, map[string]interface{}{
				"type":          "playlist",
				"id":            strconv.Itoa(int(top.ID)),
				"title":         formatTopicPlaylistTitle(top.Name, top.ID),
				"subtitle":      formatTopicSubtitle(top.TrackCount),
				"thumbnail_url": s.topicArtworkURL(ctx, top.ID, top.ArtworkURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "library:playlists",
			"title":    "Playlists",
			"items":    items,
		})
	}

	// 3. Albums
	if albums, _, err := s.db.ListAlbums(ctx, 0, 50); err == nil && len(albums) > 0 {
		items := make([]map[string]interface{}, 0, len(albums))
		for _, alb := range albums {
			items = append(items, map[string]interface{}{
				"type":          "album",
				"id":            alb.ID,
				"title":         alb.Title,
				"subtitle":      alb.Artist,
				"thumbnail_url": s.albumArtworkURL(ctx, alb.Title, alb.CoverURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "library:albums",
			"title":    "Albums",
			"items":    items,
		})
	}

	// 4. Artists
	if artists, _, err := s.db.ListArtists(ctx, 0, 50); err == nil && len(artists) > 0 {
		items := make([]map[string]interface{}, 0, len(artists))
		for _, art := range artists {
			items = append(items, map[string]interface{}{
				"type":          "artist",
				"id":            art.ID,
				"title":         art.Name,
				"subtitle":      fmt.Sprintf("%d tracks", art.TrackCount),
				"thumbnail_url": s.artistArtworkURL(ctx, art.Name, art.ThumbURL),
			})
		}
		sections = append(sections, map[string]interface{}{
			"id":       "library:artists",
			"title":    "Artists",
			"items":    items,
		})
	}

	budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
		"account":  account,
		"sections": sections,
	})
}

func (s *Server) handleStreamXSearch(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{"items": []interface{}{}})
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	if query == "" {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{"items": []interface{}{}})
		return
	}

	limit := parseQueryInt(r, "limit", 20)
	if limit > 50 {
		limit = 50
	}
	tracks, err := s.db.SearchTracks(r.Context(), query, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	items := make([]map[string]interface{}, 0, len(tracks))
	for _, tr := range tracks {
		items = append(items, s.trackToStreamXItem(tr))
	}

	budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
		"items": items,
	})
}


func (s *Server) handleStreamXDetail(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)
	ctx := r.Context()

	if s.db == nil {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"title":         id,
			"subtitle":      "",
			"thumbnail_url": "",
			"kind":          kind,
			"items":         []interface{}{},
			"next_offset":   nil,
		})
		return
	}

	switch kind {
	case "playlist":
		cleanID := strings.TrimPrefix(id, "topic_")
		topicID, err := strconv.Atoi(cleanID)
		var items []map[string]interface{}
		var title, subtitle, thumbURL string
		var nextOffset interface{} = nil

		if err == nil && topicID > 0 {
			top, _ := s.db.GetTopic(ctx, s.chatID, int32(topicID))
			rawName := strconv.Itoa(int(topicID))
			if top != nil && top.Name != "" {
				rawName = top.Name
				thumbURL = s.formatThumbnailURL(top.ArtworkURL)
			}
			title = formatTopicPlaylistTitle(rawName, int32(topicID))
			tracks, err := s.db.ListTracksByTopic(ctx, s.chatID, int32(topicID), offset, limit)
			if err != nil {
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
				return
			}
			subtitle = formatTopicSubtitle(len(tracks))
			items = make([]map[string]interface{}, 0, len(tracks))
			for _, tr := range tracks {
				items = append(items, s.trackToStreamXItem(tr))
			}
			if thumbURL == "" && len(tracks) > 0 {
				thumbURL = s.trackArtworkURL(tracks[0])
			}
			if len(tracks) == limit {
				nextOffset = offset + limit
			}
		} else {
			// User-created playlist
			pl, err := s.db.GetPlaylist(ctx, id)
			if err != nil || pl == nil {
				respondJSON(w, http.StatusNotFound, map[string]interface{}{"error": "playlist not found"})
				return
			}
			title = pl.Title
			subtitle = pl.Subtitle
			thumbURL = s.formatThumbnailURL(pl.ArtworkURL)
			tracks, err := s.db.GetPlaylistTracks(ctx, id, offset, limit)
			if err != nil {
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
				return
			}
			items = make([]map[string]interface{}, 0, len(tracks))
			for _, tr := range tracks {
				items = append(items, s.trackToStreamXItem(tr))
			}
			if thumbURL == "" && len(tracks) > 0 {
				thumbURL = s.trackArtworkURL(tracks[0])
			}
			if len(tracks) == limit {
				nextOffset = offset + limit
			}
		}

		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"title":         title,
			"subtitle":      subtitle,
			"thumbnail_url": thumbURL,
			"kind":          "playlist",
			"items":         items,
			"next_offset":   nextOffset,
		})

	case "album":
		tracks, err := s.db.GetAlbumTracks(ctx, id, limit)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
			return
		}
		items := make([]map[string]interface{}, 0, len(tracks))
		var artist, thumbURL string
		for _, tr := range tracks {
			items = append(items, s.trackToStreamXItem(tr))
			if artist == "" && tr.Artist != "" {
				artist = tr.Artist
			}
			if thumbURL == "" {
				thumbURL = s.trackArtworkURL(tr)
			}
		}
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"title":         id,
			"subtitle":      artist,
			"thumbnail_url": thumbURL,
			"kind":          "album",
			"items":         items,
			"next_offset":   nil,
		})

	case "artist":
		tracks, err := s.db.GetArtistTracks(ctx, id, limit)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
			return
		}
		items := make([]map[string]interface{}, 0, len(tracks))
		var thumbURL string
		for _, tr := range tracks {
			items = append(items, s.trackToStreamXItem(tr))
			if thumbURL == "" {
				thumbURL = s.trackArtworkURL(tr)
			}
		}
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"title":         id,
			"subtitle":      "Artist",
			"thumbnail_url": thumbURL,
			"kind":          "artist",
			"items":         items,
			"next_offset":   nil,
		})

	case "collection":
		var items []map[string]interface{}
		var title string
		var tracks []*models.Track
		var nextOffset interface{} = nil

		if id == "library" || id == "library:playlists" || id == "playlists" {
			title = "Playlists"
			topics, err := s.db.ListTopics(ctx, s.chatID)
			if err == nil {
				items = make([]map[string]interface{}, 0, len(topics))
				for _, top := range topics {
					items = append(items, map[string]interface{}{
						"type":          "playlist",
						"id":            strconv.Itoa(int(top.ID)),
						"title":         formatTopicPlaylistTitle(top.Name, top.ID),
						"subtitle":      formatTopicSubtitle(top.TrackCount),
						"thumbnail_url": s.topicArtworkURL(ctx, top.ID, top.ArtworkURL),
					})
				}
				if customPls, err := s.db.ListAllPlaylists(ctx, s.chatID); err == nil {
					for _, pl := range customPls {
						items = append(items, map[string]interface{}{
							"type":          "playlist",
							"id":            pl.ID,
							"title":         pl.Title,
							"subtitle":      fmt.Sprintf("%d tracks", pl.TrackCount),
							"thumbnail_url": s.playlistArtworkURL(ctx, pl.ID, pl.ArtworkURL),
						})
					}
				}
			}
		} else if id == "library:albums" || id == "albums" {
			title = "Albums"
			albums, _, _ := s.db.ListAlbums(ctx, offset, limit)
			items = make([]map[string]interface{}, 0, len(albums))
			for _, alb := range albums {
				items = append(items, map[string]interface{}{
					"type":          "album",
					"id":            alb.ID,
					"title":         alb.Title,
					"subtitle":      alb.Artist,
					"thumbnail_url": s.albumArtworkURL(ctx, alb.Title, alb.CoverURL),
				})
			}
		} else if id == "library:artists" || id == "artists" {
			title = "Artists"
			artists, _, _ := s.db.ListArtists(ctx, offset, limit)
			items = make([]map[string]interface{}, 0, len(artists))
			for _, art := range artists {
				items = append(items, map[string]interface{}{
					"type":          "artist",
					"id":            art.ID,
					"title":         art.Name,
					"subtitle":      fmt.Sprintf("%d tracks", art.TrackCount),
					"thumbnail_url": s.artistArtworkURL(ctx, art.Name, art.ThumbURL),
				})
			}
		} else if id == "liked" || id == "daily" {
			tracks, _ = s.db.GetRandomTracks(ctx, limit)
			title = "Daily Mix"
			items = make([]map[string]interface{}, 0, len(tracks))
			for _, tr := range tracks {
				items = append(items, s.trackToStreamXItem(tr))
			}
		} else {
			tracks, _, _ = s.db.ListTracks(ctx, offset, limit)
			title = "Your Library"
			items = make([]map[string]interface{}, 0, len(tracks))
			for _, tr := range tracks {
				items = append(items, s.trackToStreamXItem(tr))
			}
			if len(tracks) == limit {
				nextOffset = offset + limit
			}
		}

		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"title":         title,
			"subtitle":      "",
			"thumbnail_url": "",
			"kind":          "collection",
			"items":         items,
			"next_offset":   nextOffset,
		})


	default:
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": fmt.Sprintf("Unsupported kind: %s", kind),
		})
	}
}

func (s *Server) handleStreamXStream(w http.ResponseWriter, r *http.Request) {
	trackID := strings.TrimSpace(r.URL.Query().Get("id"))
	if trackID == "" {
		trackID = strings.TrimSpace(r.URL.Query().Get("track_id"))
	}
	if trackID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "Track ID required",
		})
		return
	}

	streamURL := fmt.Sprintf("http://127.0.0.1:%d/stream/%s?token=%s", s.port, trackID, s.token)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"url": streamURL,
	})
}

func (s *Server) handleStreamXSaved(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"saved": false,
	})
}

func (s *Server) handleStreamXSave(w http.ResponseWriter, r *http.Request) {
	savedStr := r.URL.Query().Get("saved")
	saved := savedStr == "true" || savedStr == "1"
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"saved": saved,
	})
}

func (s *Server) handleStreamXLyrics(w http.ResponseWriter, r *http.Request) {
	trackID := strings.TrimSpace(r.URL.Query().Get("track_id"))
	if trackID == "" {
		trackID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if trackID == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}

	var plainLyrics, syncedLyrics string
	if s.db != nil {
		if tr, err := s.db.GetTrack(r.Context(), trackID); err == nil && tr != nil {
			plainLyrics = tr.Lyrics
			syncedLyrics = tr.SyncedLyrics
		}
	}

	resp := make(map[string]interface{})
	if plainLyrics != "" {
		resp["plain"] = plainLyrics
	}
	if syncedLyrics != "" {
		resp["synced"] = syncedLyrics
	}
	respondJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStreamXPlaylists(w http.ResponseWriter, r *http.Request) {
	offset := parseQueryInt(r, "offset", 0)
	limit := parseQueryInt(r, "limit", 50)
	if limit > 50 {
		limit = 50
	}
	if limit <= 0 {
		limit = 20
	}

	folder := r.URL.Query().Get("folder")

	if s.db == nil {
		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"items":       []interface{}{},
			"folders":     []string{},
			"next_offset": -1,
		})
		return
	}

	// Case 1: Client queries a specific folder (Topic)
	if folder != "" {
		cleanID := strings.TrimPrefix(folder, "topic_")
		topicID, _ := strconv.Atoi(cleanID)

		var items []map[string]interface{}
		// 1. Primary topic playlist
		if top, err := s.db.GetTopic(r.Context(), s.chatID, int32(topicID)); err == nil && top != nil {
			items = append(items, map[string]interface{}{
				"type":          "playlist",
				"id":            strconv.Itoa(int(top.ID)),
				"title":         formatTopicPlaylistTitle(top.Name, top.ID),
				"subtitle":      formatTopicSubtitle(top.TrackCount),
				"thumbnail_url": s.topicArtworkURL(r.Context(), top.ID, top.ArtworkURL),
			})
		}
		// 2. Custom user playlists in this folder
		if customPls, err := s.db.ListPlaylistsByTopic(r.Context(), s.chatID, int32(topicID)); err == nil {
			for _, pl := range customPls {
				items = append(items, map[string]interface{}{
					"type":          "playlist",
					"id":            pl.ID,
					"title":         pl.Title,
					"subtitle":      fmt.Sprintf("%d tracks", pl.TrackCount),
					"thumbnail_url": s.playlistArtworkURL(r.Context(), pl.ID, pl.ArtworkURL),
				})
			}
		}

		budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
			"items":       items,
			"folders":     []string{},
			"next_offset": -1,
		})
		return
	}

	// Case 2: Root view (folder == "")
	// Return topic folders in "folders", plus all playlists in "items"
	topics, err := s.db.ListTopics(r.Context(), s.chatID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	total := len(topics)
	var pagedTopics []*models.Topic
	if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		pagedTopics = topics[offset:end]
	}

	items := make([]map[string]interface{}, 0, len(pagedTopics))
	folders := make([]string, 0, len(pagedTopics))
	for _, top := range pagedTopics {
		items = append(items, map[string]interface{}{
			"type":          "playlist",
			"id":            strconv.Itoa(int(top.ID)),
			"title":         formatTopicPlaylistTitle(top.Name, top.ID),
			"subtitle":      formatTopicSubtitle(top.TrackCount),
			"thumbnail_url": s.topicArtworkURL(r.Context(), top.ID, top.ArtworkURL),
		})
		folders = append(folders, strconv.Itoa(int(top.ID)))
	}

	nextOffset := -1
	if offset+len(pagedTopics) < total {
		nextOffset = offset + len(pagedTopics)
	}

	budgetResponseJSON(w, http.StatusOK, map[string]interface{}{
		"items":       items,
		"folders":     folders,
		"next_offset": nextOffset,
	})
}

func (s *Server) handleStreamXCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	if title == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "missing title"})
		return
	}
	plID := fmt.Sprintf("pl_%d", time.Now().UnixNano())
	pl := &models.Playlist{
		ID:        plID,
		ChatID:    s.chatID,
		Title:     title,
		Subtitle:  "Custom Playlist",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.db.CreatePlaylist(r.Context(), pl); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":      plID,
		"warning": "",
	})
}

func (s *Server) handleStreamXAddToPlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	tracksJSON := r.URL.Query().Get("tracks")
	var trackIDs []string
	if tracksJSON != "" {
		_ = json.Unmarshal([]byte(tracksJSON), &trackIDs)
	}
	for i, trID := range trackIDs {
		_ = s.db.AddTrackToPlaylist(r.Context(), id, trID, i)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *Server) handleStreamXRemoveFromPlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	trackID := r.URL.Query().Get("track")
	_ = s.db.RemoveTrackFromPlaylist(r.Context(), id, trackID)
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

const maxBudgetResponseBytes = 200 * 1024 // 200 KB safe ceiling under host 256 KB cap

func budgetResponseJSON(w http.ResponseWriter, status int, resp map[string]interface{}) {
	payload, err := json.Marshal(resp)
	if err == nil && len(payload) <= maxBudgetResponseBytes {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(payload)
		return
	}

	// Truncate "items" if present to stay under 200 KB
	if rawItems, ok := resp["items"].([]map[string]interface{}); ok && len(rawItems) > 0 {
		for i := len(rawItems) - 1; i >= 0; i-- {
			resp["items"] = rawItems[:i]
			if i == 0 {
				resp["items"] = []map[string]interface{}{}
			}
			candidate, err := json.Marshal(resp)
			if err == nil && len(candidate) <= maxBudgetResponseBytes {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(status)
				_, _ = w.Write(candidate)
				return
			}
		}
	}

	// Truncate "sections" if present
	if rawSections, ok := resp["sections"].([]map[string]interface{}); ok && len(rawSections) > 0 {
		for i := len(rawSections) - 1; i >= 0; i-- {
			resp["sections"] = rawSections[:i]
			candidate, err := json.Marshal(resp)
			if err == nil && len(candidate) <= maxBudgetResponseBytes {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(status)
				_, _ = w.Write(candidate)
				return
			}
		}
	}

	respondJSON(w, status, resp)
}


func (s *Server) trackToStreamXItem(tr *models.Track) map[string]interface{} {
	artist := tr.Artist
	if artist == "" {
		artist = "Unknown Artist"
	}
	title := tr.Title
	if title == "" {
		title = tr.FileName
		if title == "" {
			title = fmt.Sprintf("Track %s", tr.ID)
		}
	}
	thumbURL := s.trackArtworkURL(tr)
	if thumbURL == "" && s.enrichment != nil {
		s.enrichment.Enqueue(tr.ID)
	}

	artistID := ""
	if tr.Artist != "" {
		artistID = metadata.ArtistID(tr.Artist)
	}
	albumID := ""
	if tr.Album != "" {
		albumID = metadata.AlbumIDWithArtist(tr.Album, tr.Artist, tr.Year)
	}

	return map[string]interface{}{
		"type":          "track",
		"id":            tr.ID,
		"title":         title,
		"subtitle":      artist,
		"artist":        artist,
		"artist_id":     artistID,
		"album":         tr.Album,
		"album_id":      albumID,
		"thumbnail_url": thumbURL,
		"duration_sec":  tr.DurationSec,
		"explicit":      false,
	}
}
