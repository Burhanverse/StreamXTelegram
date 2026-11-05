package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

var (
	ErrFileReferenceExpired = errors.New("FILE_REFERENCE_EXPIRED")
	ErrWorkerNotReady       = errors.New("worker client is not ready")
)

// WorkerStatsListener enables recording MTProto download latencies, flood waits, and errors.
type WorkerStatsListener interface {
	RecordDownload(ctx context.Context, botIndex int, offset int64, limit int, duration time.Duration, err error)
	RecordFloodWait(ctx context.Context, botIndex int, wait time.Duration)
	RecordError(ctx context.Context, botIndex int, errType string)
}

// ClientWorker manages a single MTProto client connection in the pool.
type ClientWorker struct {
	ID                  int64
	Index               int
	Token               string
	Username            string
	FirstName           string
	Bot                 bool
	Ready               bool
	Workload            int64
	cooldownUntilNano   int64
	consecutiveFailures int32
	Client              *telegram.Client
	API                 *tg.Client
	db                  *database.DB
	statsListener       WorkerStatsListener

	channelAccessHashes map[int64]int64
	channelAccessMu     sync.RWMutex

	locationCache       map[int32]*models.WorkerLocation
	locationMu          sync.RWMutex
}

// SetStatsListener attaches a telemetry collector to this worker.
func (w *ClientWorker) SetStatsListener(l WorkerStatsListener) {
	w.statsListener = l
}

// IsCoolingDown returns true if the worker is currently undergoing a FLOOD_WAIT or circuit cooldown.
func (w *ClientWorker) IsCoolingDown() bool {
	return time.Now().UnixNano() < atomic.LoadInt64(&w.cooldownUntilNano)
}

// CooldownRemaining returns the time left before this worker can accept new tasks.
func (w *ClientWorker) CooldownRemaining() time.Duration {
	rem := atomic.LoadInt64(&w.cooldownUntilNano) - time.Now().UnixNano()
	if rem <= 0 {
		return 0
	}
	return time.Duration(rem)
}

// MarkCooldown puts the worker into a cooldown state for the specified duration.
func (w *ClientWorker) MarkCooldown(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	atomic.StoreInt64(&w.cooldownUntilNano, until)
}

// MarkSuccess clears consecutive failure counters after a successful request.
func (w *ClientWorker) MarkSuccess() {
	atomic.StoreInt32(&w.consecutiveFailures, 0)
}

// MarkFailure increments the failure count and activates a circuit breaker if errors repeat.
func (w *ClientWorker) MarkFailure() {
	fails := atomic.AddInt32(&w.consecutiveFailures, 1)
	if fails >= 3 {
		// Circuit breaker: back off for 5 seconds
		w.MarkCooldown(5 * time.Second)
	}
}

// NewClientWorker creates an unstarted MTProto worker instance.
func NewClientWorker(index int, token string, client *telegram.Client, db *database.DB) *ClientWorker {
	var api *tg.Client
	if client != nil {
		api = client.API()
	}
	return &ClientWorker{
		Index:               index,
		Token:               token,
		Client:              client,
		API:                 api,
		db:                  db,
		channelAccessHashes: make(map[int64]int64),
		locationCache:       make(map[int32]*models.WorkerLocation),
	}
}

// BotIDString returns the identifier for this worker (Telegram user ID or token prefix).
func (w *ClientWorker) BotIDString() string {
	if w.ID != 0 {
		return fmt.Sprintf("%d", w.ID)
	}
	parts := strings.Split(w.Token, ":")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return fmt.Sprintf("worker_%d", w.Index)
}

func (w *ClientWorker) GetChannelAccessHash(channelID int64) int64 {
	w.channelAccessMu.RLock()
	defer w.channelAccessMu.RUnlock()
	return w.channelAccessHashes[channelID]
}

func (w *ClientWorker) SetChannelAccessHash(channelID, accessHash int64) {
	w.channelAccessMu.Lock()
	defer w.channelAccessMu.Unlock()
	w.channelAccessHashes[channelID] = accessHash
}

func normalizeChannelID(chatID int64) (int64, bool) {
	if chatID <= -1000000000000 {
		return -chatID - 1000000000000, true
	}
	if chatID > 1000000000 {
		return chatID, true
	}
	return chatID, false
}

// ResolveChannelAccessHash ensures this bot has resolved the access hash for the channel.
func (w *ClientWorker) ResolveChannelAccessHash(ctx context.Context, channelID int64) (int64, error) {
	if hash := w.GetChannelAccessHash(channelID); hash != 0 {
		return hash, nil
	}
	if w.API == nil {
		return 0, fmt.Errorf("worker API is not initialized")
	}

	chats, err := w.API.MessagesGetChats(ctx, []int64{channelID})
	if err == nil {
		for _, chat := range chats.GetChats() {
			if ch, ok := chat.(*tg.Channel); ok && ch.ID == channelID {
				w.SetChannelAccessHash(channelID, ch.AccessHash)
				return ch.AccessHash, nil
			}
		}
	}

	channels, err := w.API.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: channelID},
	})
	if err == nil {
		for _, chat := range channels.GetChats() {
			if ch, ok := chat.(*tg.Channel); ok && ch.ID == channelID {
				w.SetChannelAccessHash(channelID, ch.AccessHash)
				return ch.AccessHash, nil
			}
		}
	}

	return 0, fmt.Errorf("could not resolve channel access hash for %d", channelID)
}

// FetchFileLocation queries the message for this worker and returns the session-specific location.
func (w *ClientWorker) FetchFileLocation(ctx context.Context, chatID int64, msgID int32, forceFresh bool) (*tg.InputDocumentFileLocation, int64, string, error) {
	if !w.Ready || w.API == nil {
		return nil, 0, "", ErrWorkerNotReady
	}

	if !forceFresh {
		// 1. Check in-memory cache
		w.locationMu.RLock()
		cachedLoc, ok := w.locationCache[msgID]
		w.locationMu.RUnlock()

		if !ok && w.db != nil {
			// 2. Check SQLite persistent cache
			if dbLoc, err := w.db.GetWorkerLocation(ctx, w.BotIDString(), msgID); err == nil && dbLoc != nil {
				cachedLoc = dbLoc
				w.locationMu.Lock()
				w.locationCache[msgID] = dbLoc
				w.locationMu.Unlock()
			}
		}

		if cachedLoc != nil && len(cachedLoc.FileReference) > 0 && cachedLoc.DocID > 0 {
			return &tg.InputDocumentFileLocation{
				ID:            cachedLoc.DocID,
				AccessHash:    cachedLoc.AccessHash,
				FileReference: cachedLoc.FileReference,
			}, 0, "", nil
		}
	}

	// 3. Resolve from Telegram MTProto
	channelID, isChannel := normalizeChannelID(chatID)
	var messages []tg.MessageClass

	if isChannel {
		accessHash, err := w.ResolveChannelAccessHash(ctx, channelID)
		if err != nil {
			return nil, 0, "", err
		}

		res, err := w.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channelID, AccessHash: accessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(msgID)}},
		})
		if err != nil {
			if strings.Contains(strings.ToUpper(err.Error()), "FILE_MIGRATE") {
				if w.statsListener != nil {
					w.statsListener.RecordError(ctx, w.Index, "FILE_MIGRATE")
				}
			}
			if wait, isFlood := ParseFloodWait(err); isFlood {
				if w.statsListener != nil {
					w.statsListener.RecordFloodWait(ctx, w.Index, wait)
				}
				log.Warnf("Worker #%d hit FLOOD_WAIT (%v) fetching message %d", w.Index, wait, msgID)
				_ = SleepWithContext(ctx, wait)
			}
			return nil, 0, "", fmt.Errorf("ChannelsGetMessages: %w", err)
		}
		messages = extractMessagesList(res)
	} else {
		res, err := w.API.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: int(msgID)}})
		if err != nil {
			return nil, 0, "", fmt.Errorf("MessagesGetMessages: %w", err)
		}
		messages = extractMessagesList(res)
	}

	for _, m := range messages {
		msg, ok := m.(*tg.Message)
		if !ok || msg.Media == nil {
			continue
		}
		mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok || mediaDoc.Document == nil {
			continue
		}
		doc, ok := mediaDoc.Document.(*tg.Document)
		if !ok {
			continue
		}

		location := &tg.InputDocumentFileLocation{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		}

		workerLoc := &models.WorkerLocation{
			BotID:         w.BotIDString(),
			MsgID:         msgID,
			DocID:         doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
			DCID:          int32(doc.DCID),
			UpdatedAt:     time.Now(),
		}

		w.locationMu.Lock()
		w.locationCache[msgID] = workerLoc
		w.locationMu.Unlock()

		if w.db != nil {
			go func() {
				_ = w.db.SaveWorkerLocation(context.Background(), workerLoc)
			}()
		}

		return location, doc.Size, doc.MimeType, nil
	}

	return nil, 0, "", fmt.Errorf("no audio document found in message %d", msgID)
}

func extractMessagesList(res tg.MessagesMessagesClass) []tg.MessageClass {
	if res == nil {
		return nil
	}
	if mod, ok := res.AsModified(); ok {
		return mod.GetMessages()
	}
	return nil
}

// DownloadChunk performs an UploadGetFile request with precise boundary alignment.
func (w *ClientWorker) DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	if !w.Ready || w.API == nil {
		return nil, ErrWorkerNotReady
	}

	atomic.AddInt64(&w.Workload, 1)
	defer atomic.AddInt64(&w.Workload, -1)

	// Telegram MTProto constraints:
	// - offset must be divisible by 4096 (4 KB)
	// - chunk must not exceed or cross a 1 MB boundary
	const oneMB int64 = 1024 * 1024
	alignedOffset := (offset / 4096) * 4096
	skipBytes := offset - alignedOffset

	bytesToBoundary := oneMB - (alignedOffset % oneMB)
	if int64(limit) > bytesToBoundary {
		limit = int(bytesToBoundary)
	}

	req := &tg.UploadGetFileRequest{
		Precise:  true,
		Location: location,
		Offset:   alignedOffset,
		Limit:    limit,
	}

	var res tg.UploadFileClass
	var err error

	for attempt := 0; attempt < 3; attempt++ {
		start := time.Now()
		res, err = w.API.UploadGetFile(ctx, req)
		dur := time.Since(start)

		if w.statsListener != nil {
			w.statsListener.RecordDownload(ctx, w.Index, alignedOffset, limit, dur, err)
		}

		if err == nil {
			break
		}

		if strings.Contains(strings.ToUpper(err.Error()), "FILE_REFERENCE") {
			if w.statsListener != nil {
				w.statsListener.RecordError(ctx, w.Index, "FILE_REFERENCE_EXPIRED")
			}
			return nil, ErrFileReferenceExpired
		}

		if strings.Contains(strings.ToUpper(err.Error()), "FILE_MIGRATE") {
			if w.statsListener != nil {
				w.statsListener.RecordError(ctx, w.Index, "FILE_MIGRATE")
			}
		}

		if wait, isFlood := ParseFloodWait(err); isFlood {
			w.MarkCooldown(wait)
			if w.statsListener != nil {
				w.statsListener.RecordFloodWait(ctx, w.Index, wait)
			}
			log.Warnf("Worker #%d UploadGetFile hit FLOOD_WAIT (%v) at offset %d", w.Index, wait, offset)
			priority := models.PriorityFromContext(ctx)
			if priority == models.PriorityPlayback && wait > 1*time.Second {
				// Do not block playback hot path; route to another worker immediately
				return nil, err
			}
			if sleepErr := SleepWithContext(ctx, wait); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return nil, ctx.Err()
		}

		jitter := 50*time.Millisecond + time.Duration((alignedOffset+int64(attempt*37))%100)*time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(jitter):
		}
	}

	if err != nil {
		return nil, err
	}

	switch f := res.(type) {
	case *tg.UploadFile:
		data := f.Bytes
		if skipBytes > 0 {
			if int64(len(data)) <= skipBytes {
				return []byte{}, nil
			}
			data = data[skipBytes:]
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unexpected UploadGetFile type: %T", res)
	}
}
