package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

var (
	ErrNoBackupFound    = errors.New("no musix backup found in telegram topic")
	ErrInvalidBackupDoc = errors.New("backup document is corrupted or not a valid sqlite database")
)

// BackupService handles exporting and uploading SQLite backups to Telegram,
// as well as restoring snapshots from Telegram with last-write-wins semantics.
type BackupService struct {
	pool *Pool
	db   *database.DB
}

// NewBackupService creates a backup and restore service.
func NewBackupService(pool *Pool, db *database.DB) *BackupService {
	return &BackupService{
		pool: pool,
		db:   db,
	}
}

// Backup creates a clean VACUUM INTO snapshot of library.db and uploads it to the specified topic.
// No credentials or tokens are stored in the database.
func (b *BackupService) Backup(ctx context.Context, chatID int64, topicID int32) (int32, error) {
	primary := b.pool.Primary()
	if primary == nil || !primary.Ready {
		return 0, ErrWorkerNotReady
	}

	channelID, isChannel := normalizeChannelID(chatID)
	if !isChannel {
		return 0, fmt.Errorf("chat ID %d is not a valid supergroup/channel", chatID)
	}

	accessHash, err := primary.ResolveChannelAccessHash(ctx, channelID)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve channel access hash: %w", err)
	}

	// 1. Create a clean vacuumed snapshot
	tempDir := filepath.Join(filepath.Dir(b.db.Path()), "backups")
	_ = os.MkdirAll(tempDir, 0700)
	tempFile := filepath.Join(tempDir, fmt.Sprintf("backup_%d.db", time.Now().UnixNano()))
	defer os.Remove(tempFile)

	if err := b.db.ExportBackup(tempFile); err != nil {
		return 0, fmt.Errorf("failed to export vacuumed backup: %w", err)
	}

	// 2. Upload file parts via MTProto uploader
	u := uploader.NewUploader(primary.API)
	inputFile, err := u.FromPath(ctx, tempFile)
	if err != nil {
		return 0, fmt.Errorf("failed to upload backup file to telegram: %w", err)
	}

	// 3. Send media to Telegram System topic
	now := time.Now()
	caption := fmt.Sprintf("#musix_backup v1 timestamp=%d", now.Unix())

	var randomID int64
	_ = binary.Read(rand.Reader, binary.LittleEndian, &randomID)

	peer := &tg.InputPeerChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	var replyTo tg.InputReplyToClass
	if topicID > 1 {
		replyTo = &tg.InputReplyToMessage{
			ReplyToMsgID: int(topicID),
		}
	}

	media := &tg.InputMediaUploadedDocument{
		File:      inputFile,
		MimeType:  "application/x-sqlite3",
		ForceFile: true,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("library_backup_%d.db", now.Unix())},
		},
	}

	updates, err := primary.API.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		ReplyTo:  replyTo,
		Media:    media,
		Message:  caption,
		RandomID: randomID,
	})
	if err != nil {
		return 0, fmt.Errorf("MessagesSendMedia failed: %w", err)
	}

	msgID := extractMessageIDFromUpdates(updates)
	log.Infof("Database backup uploaded successfully to topic %d (msg_id: %d)", topicID, msgID)

	_ = b.db.SaveBackupRecord(ctx, &models.BackupRecord{
		MsgID:     msgID,
		ChatID:    channelID,
		TopicID:   topicID,
		Caption:   caption,
		CreatedAt: now,
	})
	return msgID, nil
}

// Restore downloads the latest backup from the Telegram topic and applies it (last-write-wins).
func (b *BackupService) Restore(ctx context.Context, chatID int64, topicID int32) error {
	primary := b.pool.Primary()
	if primary == nil || !primary.Ready {
		return ErrWorkerNotReady
	}

	channelID, isChannel := normalizeChannelID(chatID)
	if !isChannel {
		return fmt.Errorf("chat ID %d is not a valid supergroup/channel", chatID)
	}

	accessHash, err := primary.ResolveChannelAccessHash(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to resolve channel access hash: %w", err)
	}

	peer := &tg.InputPeerChannel{
		ChannelID:  channelID,
		AccessHash: accessHash,
	}

	var candidateMessages []tg.MessageClass

	// 1. Thread inspection for forum topics (works for bots in supergroup topics)
	if topicID > 1 {
		repliesRes, err := primary.API.MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{
			Peer:  peer,
			MsgID: int(topicID),
			Limit: 50,
		})
		if err == nil {
			candidateMessages = append(candidateMessages, extractMessagesList(repliesRes)...)
		}
	}

	// 2. Check local database record for indexed backup
	if backupRec, err := b.db.GetLatestBackup(ctx, channelID, topicID); err == nil && backupRec != nil && backupRec.MsgID > 0 {
		res, err := primary.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channelID, AccessHash: accessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(backupRec.MsgID)}},
		})
		if err == nil {
			candidateMessages = append(candidateMessages, extractMessagesList(res)...)
		}
	}

	// 3. Fallback: Search for #musix_backup (works if user account or admin permissions)
	if len(candidateMessages) == 0 {
		searchReq := &tg.MessagesSearchRequest{
			Peer:   peer,
			Q:      "#musix_backup",
			Filter: &tg.InputMessagesFilterDocument{},
			Limit:  10,
		}
		if topicID > 1 {
			searchReq.SetTopMsgID(int(topicID))
		}

		res, err := primary.API.MessagesSearch(ctx, searchReq)
		if err == nil {
			candidateMessages = append(candidateMessages, extractMessagesList(res)...)
		}
	}

	// 4. Fallback: Get recent history if still nothing
	if len(candidateMessages) == 0 {
		histReq := &tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: 30,
		}
		histRes, histErr := primary.API.MessagesGetHistory(ctx, histReq)
		if histErr == nil {
			candidateMessages = append(candidateMessages, extractMessagesList(histRes)...)
		}
	}

	// 3. Find the newest valid backup document
	var bestDoc *tg.Document
	var bestMsgID int

	for _, m := range candidateMessages {
		msg, ok := m.(*tg.Message)
		if !ok || msg.Media == nil {
			continue
		}

		if !strings.Contains(msg.Message, "#musix_backup") {
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

		if msg.ID > bestMsgID {
			bestMsgID = msg.ID
			bestDoc = doc
		}
	}

	if bestDoc == nil {
		return ErrNoBackupFound
	}

	// 4. Download document
	tempDir := filepath.Join(filepath.Dir(b.db.Path()), "restored")
	_ = os.MkdirAll(tempDir, 0700)
	tempFile := filepath.Join(tempDir, fmt.Sprintf("restored_%d.db", time.Now().UnixNano()))
	defer os.Remove(tempFile)

	loc := &tg.InputDocumentFileLocation{
		ID:            bestDoc.ID,
		AccessHash:    bestDoc.AccessHash,
		FileReference: bestDoc.FileReference,
	}

	d := downloader.NewDownloader()
	if _, err := d.Download(primary.API, loc).ToPath(ctx, tempFile); err != nil {
		return fmt.Errorf("failed to download backup from telegram: %w", err)
	}

	// 5. Validate SQLite Header
	header := make([]byte, 16)
	f, err := os.Open(tempFile)
	if err != nil {
		return err
	}
	_, _ = f.Read(header)
	_ = f.Close()

	if !bytes.HasPrefix(header, []byte("SQLite format 3\000")) {
		return ErrInvalidBackupDoc
	}

	// 6. Overwrite local database via last-write-wins
	if err := b.db.ImportBackup(tempFile); err != nil {
		return fmt.Errorf("failed to import restored backup: %w", err)
	}

	log.Infof("Database successfully restored from backup (Telegram msg_id: %d)", bestMsgID)
	return nil
}

func extractMessageIDFromUpdates(updates tg.UpdatesClass) int32 {
	if updates == nil {
		return 0
	}
	switch u := updates.(type) {
	case *tg.UpdateShortSentMessage:
		return int32(u.ID)
	case *tg.Updates:
		for _, upd := range u.Updates {
			switch m := upd.(type) {
			case *tg.UpdateNewChannelMessage:
				if msg, ok := m.Message.(*tg.Message); ok {
					return int32(msg.ID)
				}
			case *tg.UpdateNewMessage:
				if msg, ok := m.Message.(*tg.Message); ok {
					return int32(msg.ID)
				}
			}
		}
	}
	return 0
}
