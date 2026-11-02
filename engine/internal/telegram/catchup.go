package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"musix/engine/internal/database"
	"musix/engine/internal/metadata"
	"musix/engine/internal/models"
	"musix/engine/internal/services"
)

// CatchupService coordinates incremental channel sync, deduplication, and message scanning.
type CatchupService struct {
	pool       *Pool
	db         *database.DB
	enrichment *services.EnrichmentService
}

// NewCatchupService creates a new catch-up service using the multi-bot pool, database, and enrichment pipeline.
func NewCatchupService(pool *Pool, db *database.DB, enrichment *services.EnrichmentService) *CatchupService {
	return &CatchupService{
		pool:       pool,
		db:         db,
		enrichment: enrichment,
	}
}

// SyncChannel syncs new and edited audio tracks for a channel into the local database.
// It uses updates.getChannelDifference when pts > 0 and falls back to high-water mark scanning.
func (s *CatchupService) SyncChannel(ctx context.Context, chatID int64) error {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return ErrWorkerNotReady
	}

	channelID, isChannel := normalizeChannelID(chatID)
	if !isChannel {
		return fmt.Errorf("chat ID %d is not a valid supergroup/channel ID", chatID)
	}

	accessHash, err := primary.ResolveChannelAccessHash(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to resolve channel access hash: %w", err)
	}

	state, err := s.db.GetSyncState(ctx, channelID)
	if err != nil || state == nil {
		state = &models.SyncState{ChannelID: channelID}
	}

	// 1. Sync forum topic metadata (folders) from Telegram
	s.SyncForumTopics(ctx, channelID, accessHash)

	if state.Pts > 0 {
		tooLong, err := s.CatchupChannelDifference(ctx, channelID, accessHash, state)
		if err != nil {
			log.Warnf("CatchupChannelDifference failed (%v); falling back to high-water mark scan", err)
			return s.CatchupHighWaterMark(ctx, channelID, accessHash, state, 3)
		}
		if tooLong {
			return s.CatchupHighWaterMark(ctx, channelID, accessHash, state, 3)
		}
		return nil
	}

	// Initial sync: scan existing messages up to high-water mark
	return s.CatchupHighWaterMark(ctx, channelID, accessHash, state, 3)
}

// RescanFromScratch resets database records and sync state, re-discovers all forum topics,
// and rescans all channel messages from message ID 1 to the newest message across all topics.
func (s *CatchupService) RescanFromScratch(ctx context.Context, chatID int64) error {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return ErrWorkerNotReady
	}

	channelID, isChannel := normalizeChannelID(chatID)
	if !isChannel {
		return fmt.Errorf("chat ID %d is not a valid supergroup/channel ID", chatID)
	}

	accessHash, err := primary.ResolveChannelAccessHash(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to resolve channel access hash: %w", err)
	}

	log.Infof("Resetting database and starting full rescan of channel %d...", channelID)

	// 1. Wipe database tables
	if err := s.db.ResetAllTables(ctx); err != nil {
		return fmt.Errorf("failed to reset database tables: %w", err)
	}

	// 2. Clear location cache on primary
	primary.locationMu.Lock()
	primary.locationCache = make(map[int32]*models.WorkerLocation)
	primary.locationMu.Unlock()

	// 3. Initialize clean sync state
	state := &models.SyncState{
		ChannelID: channelID,
		Pts:       0,
		MaxMsgID:  0,
	}
	_ = s.db.SaveSyncState(ctx, state)

	// 4. Sync forum topic folders and custom #playlist mapping
	s.SyncForumTopics(ctx, channelID, accessHash)

	// 5. Scan all messages starting from message ID 1
	err = s.CatchupHighWaterMark(ctx, channelID, accessHash, state, 5)
	if err != nil {
		return fmt.Errorf("full rescan high-water mark scan failed: %w", err)
	}

	// 6. Refresh PTS
	inputChannel := &tg.InputChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}
	if fullRes, err := primary.API.ChannelsGetFullChannel(ctx, inputChannel); err == nil {
		if full, ok := fullRes.FullChat.(*tg.ChannelFull); ok && full.Pts > 0 {
			state.Pts = full.Pts
			_ = s.db.SaveSyncState(ctx, state)
		}
	}

	// 7. Enqueue all tracks for metadata enrichment
	if s.enrichment != nil {
		s.enrichment.CatchUpUnenriched(ctx)
	}

	log.Infof("Full rescan of channel %d completed successfully (indexed up to msg %d)", channelID, state.MaxMsgID)
	return nil
}

// CatchupChannelDifference fetches incremental updates since the last persisted PTS.
// Returns (true, nil) if channelDifferenceTooLong occurred, indicating high-water mark fallback is needed.
func (s *CatchupService) CatchupChannelDifference(ctx context.Context, channelID, accessHash int64, state *models.SyncState) (bool, error) {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return false, ErrWorkerNotReady
	}

	inputChannel := &tg.InputChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		res, err := primary.API.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: inputChannel,
			Filter:  &tg.ChannelMessagesFilterEmpty{},
			Pts:     state.Pts,
			Limit:   100,
		})
		if err != nil {
			if wait, isFlood := ParseFloodWait(err); isFlood {
				log.Warnf("CatchupChannelDifference FLOOD_WAIT (%v); sleeping...", wait)
				if sleepErr := SleepWithContext(ctx, wait); sleepErr != nil {
					return false, sleepErr
				}
				continue
			}
			return false, err
		}

		switch diff := res.(type) {
		case *tg.UpdatesChannelDifferenceEmpty:
			state.Pts = diff.Pts
			_ = s.db.SaveSyncState(ctx, state)
			return false, nil

		case *tg.UpdatesChannelDifferenceTooLong:
			log.Warnf("Channel %d difference too long; fallback to high-water mark scan required", channelID)
			return true, nil

		case *tg.UpdatesChannelDifference:
			state.Pts = diff.Pts

			for _, m := range diff.NewMessages {
				if msg, ok := m.(*tg.Message); ok {
					s.processMessage(ctx, msg, channelID, state)
				}
			}

			for _, upd := range diff.OtherUpdates {
				switch u := upd.(type) {
				case *tg.UpdateNewChannelMessage:
					if msg, ok := u.Message.(*tg.Message); ok {
						s.processMessage(ctx, msg, channelID, state)
					}
				case *tg.UpdateEditChannelMessage:
					if msg, ok := u.Message.(*tg.Message); ok {
						s.processMessage(ctx, msg, channelID, state)
					}
				}
			}

			_ = s.db.SaveSyncState(ctx, state)

			if diff.Final {
				return false, nil
			}

		default:
			return false, fmt.Errorf("unexpected ChannelDifference response: %T", res)
		}
	}
}

// CatchupHighWaterMark scans messages in batches of 100 starting from max_msg_id + 1.
// It terminates after consecutiveEmptyLimit consecutive empty batches to tolerate gaps from deleted messages.
func (s *CatchupService) CatchupHighWaterMark(
	ctx context.Context,
	channelID, accessHash int64,
	state *models.SyncState,
	consecutiveEmptyLimit int,
) error {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return ErrWorkerNotReady
	}

	if consecutiveEmptyLimit <= 0 {
		consecutiveEmptyLimit = 3
	}

	inputChannel := &tg.InputChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	currentID := state.MaxMsgID + 1
	consecutiveEmptyBatches := 0
	const batchSize = 100

	for consecutiveEmptyBatches < consecutiveEmptyLimit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		ids := make([]tg.InputMessageClass, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			ids = append(ids, &tg.InputMessageID{ID: int(currentID) + i})
		}

		res, err := primary.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: inputChannel,
			ID:      ids,
		})
		if err != nil {
			if wait, isFlood := ParseFloodWait(err); isFlood {
				log.Warnf("ChannelsGetMessages FLOOD_WAIT (%v) at msg %d; sleeping...", wait, currentID)
				if sleepErr := SleepWithContext(ctx, wait); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			return fmt.Errorf("ChannelsGetMessages failed at %d: %w", currentID, err)
		}

		messages := extractMessagesList(res)
		validCount := 0

		for _, m := range messages {
			msg, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			validCount++
			s.processMessage(ctx, msg, channelID, state)
		}

		if validCount == 0 {
			consecutiveEmptyBatches++
		} else {
			consecutiveEmptyBatches = 0
		}

		currentID += int32(batchSize)
		_ = s.db.SaveSyncState(ctx, state)
	}

	// Refresh PTS from channel full information
	if fullRes, err := primary.API.ChannelsGetFullChannel(ctx, inputChannel); err == nil {
		if full, ok := fullRes.FullChat.(*tg.ChannelFull); ok && full.Pts > 0 {
			state.Pts = full.Pts
			_ = s.db.SaveSyncState(ctx, state)
		}
	}

	return nil
}

// SyncForumTopics queries the supergroup forum topics and registers them as playlist folders.
func (s *CatchupService) SyncForumTopics(ctx context.Context, channelID, accessHash int64) {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return
	}

	peer := &tg.InputPeerChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	topicsRes, err := primary.API.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{
		Peer:  peer,
		Limit: 100,
	})
	if err != nil {
		log.Debugf("MessagesGetForumTopics not available or chat not a forum: %v", err)
		return
	}

	for _, tClass := range topicsRes.Topics {
		if ft, ok := tClass.(*tg.ForumTopic); ok {
			title := ft.Title
			if title == "" && ft.ID == 1 {
				title = "General"
			} else if title == "" {
				title = fmt.Sprintf("%d", ft.ID)
			}
			_ = s.db.SaveTopic(ctx, &models.Topic{
				ID:          int32(ft.ID),
				ChatID:      channelID,
				Name:        title,
				IconColor:   ft.IconColor,
				IconEmojiID: ft.IconEmojiID,
				IsFolder:    true,
				UpdatedAt:   time.Now(),
			})
			log.Infof("Discovered forum topic folder: '%s' (ID %d)", title, ft.ID)
		}
	}

	// Also search for any #playlist configuration messages in the group
	searchRes, err := primary.API.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer:   peer,
		Q:      "#playlist",
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  10,
	})
	if err == nil && searchRes != nil {
		var msgs []tg.MessageClass
		switch r := searchRes.(type) {
		case *tg.MessagesMessages:
			msgs = r.Messages
		case *tg.MessagesMessagesSlice:
			msgs = r.Messages
		case *tg.MessagesChannelMessages:
			msgs = r.Messages
		}
		for _, mClass := range msgs {
			if m, ok := mClass.(*tg.Message); ok {
				for tid, name := range ParsePlaylistsMessage(m.Message) {
					_ = s.db.SaveTopic(ctx, &models.Topic{
						ID:        tid,
						ChatID:    channelID,
						Name:      name,
						IsFolder:  true,
						UpdatedAt: time.Now(),
					})
					log.Infof("Loaded custom playlist mapping from Telegram #playlist: '%s' -> Topic %d", name, tid)
				}
			}
		}
	}
}

// ParsePlaylistsMessage extracts custom topic playlist name mappings from a #playlist message.
// Supported schema:
// #playlist
//
// name: Hindi
// topicid: 3
// name: English
// topicid: 2
func ParsePlaylistsMessage(text string) map[int32]string {
	result := make(map[int32]string)
	if !strings.Contains(strings.ToLower(text), "#playlist") {
		return result
	}
	lines := strings.Split(text, "\n")
	var currentName string
	var currentTopicID int32 = -1

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "name:") || strings.HasPrefix(lower, "title:") {
			colonIdx := strings.Index(line, ":")
			if colonIdx >= 0 {
				name := strings.TrimSpace(line[colonIdx+1:])
				if currentTopicID > 0 {
					result[currentTopicID] = name
					currentTopicID = -1
					currentName = ""
				} else {
					currentName = name
				}
			}
		} else if strings.HasPrefix(lower, "topicid:") || strings.HasPrefix(lower, "topic_id:") || strings.HasPrefix(lower, "topic:") {
			colonIdx := strings.Index(line, ":")
			if colonIdx >= 0 {
				valStr := strings.TrimSpace(line[colonIdx+1:])
				if tid, err := strconv.Atoi(valStr); err == nil && tid > 0 {
					if currentName != "" {
						result[int32(tid)] = currentName
						currentName = ""
						currentTopicID = -1
					} else {
						currentTopicID = int32(tid)
					}
				}
			}
		}
	}
	return result
}

// ResolveTopicName returns the cached or resolved forum topic title.
func (s *CatchupService) ResolveTopicName(ctx context.Context, channelID int64, topicID int32) string {
	if topicID <= 0 {
		return ""
	}
	if topicID == 1 {
		return "General"
	}
	existing, err := s.db.GetTopic(ctx, channelID, topicID)
	if err == nil && existing != nil && existing.Name != "" && !strings.HasPrefix(existing.Name, "Topic ") {
		return existing.Name
	}

	if s.pool == nil {
		return strconv.Itoa(int(topicID))
	}
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready || primary.API == nil {
		return strconv.Itoa(int(topicID))
	}

	accessHash, _ := primary.ResolveChannelAccessHash(ctx, channelID)
	res, err := primary.API.MessagesGetForumTopicsByID(ctx, &tg.MessagesGetForumTopicsByIDRequest{
		Peer: &tg.InputPeerChannel{
			ChannelID:  channelID,
			AccessHash: accessHash,
		},
		Topics: []int{int(topicID)},
	})
	if err == nil && res != nil {
		for _, tClass := range res.Topics {
			if ft, ok := tClass.(*tg.ForumTopic); ok && ft.ID == int(topicID) && ft.Title != "" {
				_ = s.db.SaveTopic(ctx, &models.Topic{
					ID:          topicID,
					ChatID:      channelID,
					Name:        ft.Title,
					IconColor:   ft.IconColor,
					IconEmojiID: ft.IconEmojiID,
					IsFolder:    true,
					UpdatedAt:   time.Now(),
				})
				return ft.Title
			}
		}
	}
	return strconv.Itoa(int(topicID))
}

func (s *CatchupService) processMessage(ctx context.Context, msg *tg.Message, channelID int64, state *models.SyncState) bool {
	if int32(msg.ID) > state.MaxMsgID {
		state.MaxMsgID = int32(msg.ID)
	}

	updated := false

	// 0. Check for #musix_backup snapshot documents
	if strings.Contains(msg.Message, "#musix_backup") && msg.Media != nil {
		if mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument); ok && mediaDoc.Document != nil {
			if doc, ok := mediaDoc.Document.(*tg.Document); ok {
				var topicID int32
				if msg.ReplyTo != nil {
					if hdr, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
						if topID, ok := hdr.GetReplyToTopID(); ok {
							topicID = int32(topID)
						} else if hdr.ReplyToTopID != 0 {
							topicID = int32(hdr.ReplyToTopID)
						} else if hdr.ForumTopic && hdr.ReplyToMsgID != 0 {
							topicID = int32(hdr.ReplyToMsgID)
						}
					}
				}
				_ = s.db.SaveBackupRecord(ctx, &models.BackupRecord{
					MsgID:         int32(msg.ID),
					ChatID:        channelID,
					TopicID:       topicID,
					DocID:         doc.ID,
					AccessHash:    doc.AccessHash,
					FileReference: doc.FileReference,
					Caption:       msg.Message,
					CreatedAt:     time.Unix(int64(msg.Date), 0),
				})
				log.Infof("Indexed SQLite backup from Telegram: msg_id=%d, topic_id=%d", msg.ID, topicID)
				updated = true
			}
		}
	}

	// 0.1 Check for #playlists configuration message
	if strings.Contains(strings.ToLower(msg.Message), "#playlist") {
		for tid, name := range ParsePlaylistsMessage(msg.Message) {
			_ = s.db.SaveTopic(ctx, &models.Topic{
				ID:        tid,
				ChatID:    channelID,
				Name:      name,
				IsFolder:  true,
				UpdatedAt: time.Now(),
			})
			log.Infof("Discovered playlist mapping from message: '%s' -> Topic %d", name, tid)
			updated = true
		}
	}

	track, source, loc := ExtractAudioTrack(msg, channelID)
	if track == nil {
		return updated
	}

	// 1. Tag & Filename Sanitization
	cleanTitle, cleanArtist := metadata.CleanMetadata(track.Title, track.Artist)
	track.Title = cleanTitle
	track.Artist = cleanArtist
	track.Album = metadata.StripNoise(track.Album)

	// 2. Generate deduplication fingerprint
	track.Fingerprint = services.MetadataFingerprint(track.Title, track.Artist, track.Album, track.DurationSec)

	// 3. Conservative Deduplication: Check if track exists by file_unique_id, fingerprint, or fuzzy (title+artist+duration)
	existing, isDup, err := services.FindDuplicate(ctx, s.db, source.FileUniqueID, track.Title, track.Artist, track.Album, track.DurationSec)
	if err == nil && isDup && existing != nil {
		// Link new message/topic source to existing canonical track
		source.TrackID = existing.ID
		if err := s.db.AddTrackSource(ctx, source); err != nil {
			log.Warnf("Failed to link duplicate source %s: %v", source.TrackID, err)
		}
	} else {
		// Insert canonical track
		if err := s.db.SaveTrack(ctx, track); err != nil {
			log.Warnf("Failed to save track %s: %v", track.ID, err)
		}
		if err := s.db.AddTrackSource(ctx, source); err != nil {
			log.Warnf("Failed to add track source %s: %v", source.TrackID, err)
		}
		if s.enrichment != nil {
			s.enrichment.Enqueue(track.ID)
		}
	}

	// 4. Record/update topic playlist entry if sent in a forum topic
	if source.TopicID > 0 {
		topicName := s.ResolveTopicName(ctx, channelID, source.TopicID)
		var iconColor int
		var iconEmojiID int64
		if existingTopic, _ := s.db.GetTopic(ctx, channelID, source.TopicID); existingTopic != nil {
			iconColor = existingTopic.IconColor
			iconEmojiID = existingTopic.IconEmojiID
		}
		_ = s.db.SaveTopic(ctx, &models.Topic{
			ID:          source.TopicID,
			ChatID:      channelID,
			Name:        topicName,
			IconColor:   iconColor,
			IconEmojiID: iconEmojiID,
			IsFolder:    true,
			UpdatedAt:   time.Now(),
		})
	}

	// 5. Cache worker location
	primary := s.pool.Primary()
	if primary != nil && loc != nil {
		workerLoc := &models.WorkerLocation{
			BotID:         primary.BotIDString(),
			MsgID:         int32(msg.ID),
			DocID:         loc.ID,
			AccessHash:    loc.AccessHash,
			FileReference: loc.FileReference,
			UpdatedAt:     time.Now(),
		}
		primary.locationMu.Lock()
		primary.locationCache[int32(msg.ID)] = workerLoc
		primary.locationMu.Unlock()

		_ = s.db.SaveWorkerLocation(ctx, workerLoc)
	}

	return true
}

// ExtractAudioTrack parses a Telegram message into canonical Track, TrackSource, and Location.
// Returns nil if the message does not contain audio or audio document.
func ExtractAudioTrack(msg *tg.Message, chatID int64) (*models.Track, *models.TrackSource, *tg.InputDocumentFileLocation) {
	if msg == nil || msg.Media == nil {
		return nil, nil, nil
	}

	mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok || mediaDoc.Document == nil {
		return nil, nil, nil
	}

	doc, ok := mediaDoc.Document.(*tg.Document)
	if !ok {
		return nil, nil, nil
	}

	var title, artist, album, fileName string
	var durationSec int
	isAudio := false

	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeAudio:
			isAudio = true
			title = strings.TrimSpace(a.Title)
			artist = strings.TrimSpace(a.Performer)
			durationSec = a.Duration
		case *tg.DocumentAttributeFilename:
			fileName = strings.TrimSpace(a.FileName)
		}
	}

	mime := strings.ToLower(doc.MimeType)
	if !isAudio {
		if strings.HasPrefix(mime, "audio/") || isAudioExt(fileName) {
			isAudio = true
		}
	}

	if !isAudio {
		return nil, nil, nil
	}

	title, artist = metadata.ExtractTitleAndArtist(title, artist, fileName)
	if title == "" {
		title = fmt.Sprintf("Track %d", msg.ID)
	}
	if artist == "" {
		artist = "Unknown Artist"
	}
	if album == "" {
		album = "Telegram Music"
	}

	var topicID int32
	if msg.ReplyTo != nil {
		if hdr, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
			if topID, ok := hdr.GetReplyToTopID(); ok {
				topicID = int32(topID)
			} else if hdr.ReplyToTopID != 0 {
				topicID = int32(hdr.ReplyToTopID)
			} else if hdr.ForumTopic && hdr.ReplyToMsgID != 0 {
				topicID = int32(hdr.ReplyToMsgID)
			}
		}
	}

	trackID := fmt.Sprintf("doc_%d", doc.ID)
	createdAt := time.Unix(int64(msg.Date), 0)
	if msg.Date == 0 {
		createdAt = time.Now()
	}

	track := &models.Track{
		ID:            trackID,
		Title:         title,
		Artist:        artist,
		Album:         album,
		DurationSec:   durationSec,
		MimeType:      doc.MimeType,
		FileSize:      doc.Size,
		FileName:      fileName,
		CreatedAt:     createdAt,
		ChatID:        chatID,
		MsgID:         int32(msg.ID),
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}

	source := &models.TrackSource{
		TrackID:      trackID,
		ChatID:       chatID,
		MsgID:        int32(msg.ID),
		TopicID:      topicID,
		FileUniqueID: fmt.Sprintf("%d", doc.ID),
		CreatedAt:    createdAt,
	}

	loc := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}

	return track, source, loc
}

func isAudioExt(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3", ".m4a", ".flac", ".alac", ".wav", ".ogg", ".opus", ".aac":
		return true
	default:
		return false
	}
}

// PollIncremental performs a lightweight, incremental check for new tracks and forum topic changes.
// Returns the number of new tracks or updates ingested.
func (s *CatchupService) PollIncremental(ctx context.Context, chatID int64) (int, error) {
	primary := s.pool.Primary()
	if primary == nil || !primary.Ready {
		return 0, ErrWorkerNotReady
	}

	channelID, isChannel := normalizeChannelID(chatID)
	if !isChannel {
		return 0, fmt.Errorf("chat ID %d is not a valid channel", chatID)
	}

	accessHash, err := primary.ResolveChannelAccessHash(ctx, channelID)
	if err != nil {
		return 0, err
	}

	state, err := s.db.GetSyncState(ctx, channelID)
	if err != nil || state == nil || state.Pts == 0 {
		return 0, s.SyncChannel(ctx, chatID)
	}

	inputChannel := &tg.InputChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	res, err := primary.API.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelMessagesFilterEmpty{},
		Pts:     state.Pts,
		Limit:   100,
	})
	if err != nil {
		if _, isFlood := ParseFloodWait(err); isFlood {
			return 0, err
		}
		return 0, err
	}

	count := 0
	switch diff := res.(type) {
	case *tg.UpdatesChannelDifferenceEmpty:
		state.Pts = diff.Pts
		_ = s.db.SaveSyncState(ctx, state)
		return 0, nil

	case *tg.UpdatesChannelDifferenceTooLong:
		log.Infof("Watcher: channel difference too long; triggering catchup scan")
		return 0, s.CatchupHighWaterMark(ctx, channelID, accessHash, state, 3)

	case *tg.UpdatesChannelDifference:
		state.Pts = diff.Pts
		for _, m := range diff.NewMessages {
			if msg, ok := m.(*tg.Message); ok {
				if s.processMessage(ctx, msg, channelID, state) {
					count++
				}
			}
		}
		for _, upd := range diff.OtherUpdates {
			switch u := upd.(type) {
			case *tg.UpdateNewChannelMessage:
				if msg, ok := u.Message.(*tg.Message); ok {
					if s.processMessage(ctx, msg, channelID, state) {
						count++
					}
				}
			case *tg.UpdateEditChannelMessage:
				if msg, ok := u.Message.(*tg.Message); ok {
					if s.processMessage(ctx, msg, channelID, state) {
						count++
					}
				}
			}
		}
		_ = s.db.SaveSyncState(ctx, state)

		if count > 0 {
			// Synchronize any updated #playlist mappings
			s.SyncForumTopics(ctx, channelID, accessHash)
		}
		return count, nil
	}

	return 0, nil
}

// Watcher coordinates background periodic polling of Telegram forum updates.
type Watcher struct {
	catchup  *CatchupService
	chatID   int64
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	running  bool
}

// StartWatcher starts a background goroutine that polls the Telegram channel for new tracks and #playlist updates.
func (s *CatchupService) StartWatcher(ctx context.Context, chatID int64, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	wCtx, cancel := context.WithCancel(ctx)
	w := &Watcher{
		catchup:  s,
		chatID:   chatID,
		interval: interval,
		ctx:      wCtx,
		cancel:   cancel,
		running:  true,
	}
	go w.run()
	return w
}

// Stop terminates the background watcher.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return
	}
	w.running = false
	if w.cancel != nil {
		w.cancel()
	}
}

// IsRunning reports whether the watcher loop is active.
func (w *Watcher) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *Watcher) run() {
	log.Infof("Telegram forum background watcher started (interval: %v for chat %d)", w.interval, w.chatID)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Initial quiet check after 5 seconds to warm up
	select {
	case <-w.ctx.Done():
		return
	case <-time.After(5 * time.Second):
		if count, err := w.catchup.PollIncremental(w.ctx, w.chatID); err == nil && count > 0 {
			log.Infof("[watcher] Discovered %d new update(s) from forum on initial check", count)
		}
	}

	for {
		select {
		case <-w.ctx.Done():
			log.Infof("Telegram forum background watcher stopped")
			return
		case <-ticker.C:
			count, err := w.catchup.PollIncremental(w.ctx, w.chatID)
			if err != nil {
				log.Debugf("Watcher poll error: %v", err)
			} else if count > 0 {
				log.Infof("[watcher] Discovered %d new update(s) from forum", count)
			}
		}
	}
}
