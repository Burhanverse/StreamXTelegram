package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"musix/engine/internal/database"
	"musix/engine/internal/models"
)

// Pool manages a multi-bot pool (1 primary + N-1 workers) for MTProto streaming and catch-up.
type Pool struct {
	config           *models.Config
	db               *database.DB
	dispatcher       tg.UpdateDispatcher
	primaryWorker    *ClientWorker
	secondaryWorkers []*ClientWorker
	allWorkers       []*ClientWorker
	statsListener    WorkerStatsListener
	roundRobin       uint64
	mu               sync.RWMutex
	cancel           context.CancelFunc
	tracks           map[string]*models.Track
	tracksMu         sync.RWMutex
}

var _ Client = (*Pool)(nil)

// NewPool initializes the multi-bot pool without starting network connections.
func NewPool(cfg *models.Config, db *database.DB) (*Pool, error) {
	if cfg.ApiID <= 0 || cfg.ApiHash == "" {
		return nil, errors.New("API_ID and API_HASH are required to initialize gotd client pool")
	}

	sessionDir := filepath.Join(cfg.FilesDir, "sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		sessionDir = filepath.Join(os.TempDir(), "gotd_sessions")
		_ = os.MkdirAll(sessionDir, 0700)
	}

	dispatcher := tg.NewUpdateDispatcher()

	p := &Pool{
		config:           cfg,
		db:               db,
		dispatcher:       dispatcher,
		secondaryWorkers: make([]*ClientWorker, 0),
		allWorkers:       make([]*ClientWorker, 0),
		tracks:           make(map[string]*models.Track),
	}

	createOptions := func(sessionPath string, isPrimary bool) telegram.Options {
		opts := telegram.Options{
			NoUpdates:        !isPrimary,
			SessionStorage:   &session.FileStorage{Path: sessionPath},
			MigrationTimeout: 60 * time.Second,
			DialTimeout:      15 * time.Second,
			Resolver: dcs.Plain(dcs.PlainOptions{
				Protocol:   transport.Abridged,
				Obfuscated: true,
				Network:    "tcp4",
			}),
		}
		if isPrimary {
			opts.UpdateHandler = dispatcher
		}
		return opts
	}

	// 1. Initialize Primary Bot (index 0)
	primaryPrefix := "primary"
	if cfg.BotToken != "" {
		primaryPrefix = strings.Split(cfg.BotToken, ":")[0]
	}
	primarySessionPath := filepath.Join(sessionDir, fmt.Sprintf("bot_0_%s.json", primaryPrefix))
	primaryClient := telegram.NewClient(cfg.ApiID, cfg.ApiHash, createOptions(primarySessionPath, true))
	p.primaryWorker = NewClientWorker(0, cfg.BotToken, primaryClient, db)
	p.allWorkers = append(p.allWorkers, p.primaryWorker)

	// 2. Initialize Secondary Worker Bots (indices 1..N-1)
	poolSize := cfg.WorkerCount
	if poolSize <= 0 {
		poolSize = 3 // default small pool: 1 primary + 2 workers
	}

	for i, token := range cfg.WorkerTokens {
		token = strings.TrimSpace(token)
		if token == "" || token == cfg.BotToken {
			continue
		}
		if len(p.allWorkers) >= poolSize {
			break
		}

		idx := len(p.allWorkers)
		tokenPrefix := strings.Split(token, ":")[0]
		workerSessionPath := filepath.Join(sessionDir, fmt.Sprintf("bot_%d_%s.json", idx, tokenPrefix))
		workerClient := telegram.NewClient(cfg.ApiID, cfg.ApiHash, createOptions(workerSessionPath, false))
		worker := NewClientWorker(idx, token, workerClient, db)

		p.secondaryWorkers = append(p.secondaryWorkers, worker)
		p.allWorkers = append(p.allWorkers, worker)
		log.Infof("Registered multi-client worker bot #%d (prefix: %s)", idx, RedactSecret(token))
		_ = i
	}

	return p, nil
}

// Start connects all bots in the pool concurrently with automatic reconnection.
func (p *Pool) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	readyChan := make(chan struct{})
	var once sync.Once

	for _, w := range p.allWorkers {
		worker := w
		go func() {
			backoff := 2 * time.Second
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				err := worker.Client.Run(ctx, func(runCtx context.Context) error {
					if worker.Token != "" {
						status, err := worker.Client.Auth().Status(runCtx)
						if err == nil && !status.Authorized {
							if _, err := worker.Client.Auth().Bot(runCtx, worker.Token); err != nil {
								log.Warnf("Worker #%d auth failed: %v", worker.Index, err)
								return err
							}
						}
					}

					self, err := worker.Client.Self(runCtx)
					if err == nil && self != nil {
						p.mu.Lock()
						worker.ID = self.ID
						worker.FirstName = self.FirstName
						worker.Username = self.Username
						worker.Bot = self.Bot
						worker.Ready = true
						p.mu.Unlock()
						log.Infof("Worker #%d ready: @%s (ID: %d)", worker.Index, self.Username, self.ID)
					}

					backoff = 2 * time.Second

					if worker == p.primaryWorker {
						once.Do(func() {
							close(readyChan)
						})
					}

					<-runCtx.Done()
					return runCtx.Err()
				})

				p.mu.Lock()
				worker.Ready = false
				p.mu.Unlock()

				if ctx.Err() != nil {
					return
				}

				if err != nil {
					log.Warnf("Worker #%d disconnected: %v; reconnecting in %v", worker.Index, err, backoff)
					time.Sleep(backoff)
					if backoff < 30*time.Second {
						backoff *= 2
					}
				}
			}
		}()
	}

	select {
	case <-readyChan:
		return nil
	case <-time.After(12 * time.Second):
		// Allow background connection to continue
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop cleanly disconnects all pool clients.
func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Lock()
	for _, w := range p.allWorkers {
		w.Ready = false
	}
	p.mu.Unlock()
}

// Primary returns the primary worker responsible for updates and maintenance.
func (p *Pool) Primary() *ClientWorker {
	return p.primaryWorker
}

// Workers returns all registered workers.
func (p *Pool) Workers() []*ClientWorker {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*ClientWorker, len(p.allWorkers))
	copy(out, p.allWorkers)
	return out
}

// ReadyWorkerCount returns the number of currently authenticated and connected workers.
func (p *Pool) ReadyWorkerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, w := range p.allWorkers {
		if w.Ready {
			count++
		}
	}
	return count
}

// SetStatsListener registers a telemetry stats listener across all workers in the pool.
func (p *Pool) SetStatsListener(l WorkerStatsListener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statsListener = l
	for _, w := range p.allWorkers {
		w.SetStatsListener(l)
	}
}

// IsReady returns true if the primary bot is authenticated and connected.
func (p *Pool) IsReady() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.primaryWorker != nil && p.primaryWorker.Ready
}

// AcquireWorker selects the least-loaded ready and non-cooling worker.
// Ties are spread round-robin so the primary worker is not monopolized.
func (p *Pool) AcquireWorker() *ClientWorker {
	return p.AcquireWorkerWithPriority(models.PriorityPlayback)
}

// AcquireWorkerWithPriority selects a ready worker respecting priority lanes and worker cooldowns.
func (p *Pool) AcquireWorkerWithPriority(priority models.Priority) *ClientWorker {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var candidates []*ClientWorker
	for _, w := range p.allWorkers {
		if w.Ready && !w.IsCoolingDown() {
			candidates = append(candidates, w)
		}
	}

	if len(candidates) == 0 {
		// If all workers are cooling down, select the worker with the shortest cooldown remaining
		var leastCooling *ClientWorker
		var minRem time.Duration = 1<<62 - 1
		for _, w := range p.allWorkers {
			if w.Ready {
				rem := w.CooldownRemaining()
				if rem < minRem {
					minRem = rem
					leastCooling = w
				}
			}
		}
		if leastCooling != nil {
			return leastCooling
		}
		return p.primaryWorker
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	// Find the minimum workload among candidates
	minLoad := int64(1<<62 - 1)
	for _, w := range candidates {
		load := atomic.LoadInt64(&w.Workload)
		if load < minLoad {
			minLoad = load
		}
	}

	// Collect tied candidates
	var tied []*ClientWorker
	for _, w := range candidates {
		if atomic.LoadInt64(&w.Workload) == minLoad {
			tied = append(tied, w)
		}
	}

	// Spread ties round-robin so worker 0 is not favored over secondary workers
	idx := int(atomic.LoadUint64(&p.roundRobin) % uint64(len(tied)))
	atomic.AddUint64(&p.roundRobin, 1)
	return tied[idx]
}

// DispatchChunkDownload handles downloading a chunk with worker striping, fallback across workers,
// and automatic reference refresh.
func (p *Pool) DispatchChunkDownload(
	ctx context.Context,
	chatID int64,
	msgID int32,
	offset int64,
	limit int,
) ([]byte, error) {
	priority := models.PriorityFromContext(ctx)
	p.mu.RLock()
	workerCount := len(p.allWorkers)
	p.mu.RUnlock()

	maxAttempts := workerCount
	if maxAttempts < 2 {
		maxAttempts = 2
	}

	var lastErr error
	triedWorkers := make(map[int]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		worker := p.AcquireWorkerWithPriority(priority)
		if worker == nil || !worker.Ready {
			return nil, ErrWorkerNotReady
		}

		// Avoid immediately retrying the same worker if other workers exist
		if triedWorkers[worker.Index] && len(triedWorkers) < workerCount {
			for _, alt := range p.allWorkers {
				if alt.Ready && !alt.IsCoolingDown() && !triedWorkers[alt.Index] {
					worker = alt
					break
				}
			}
		}
		triedWorkers[worker.Index] = true

		location, _, _, err := worker.FetchFileLocation(ctx, chatID, msgID, false)
		if err != nil {
			// Fallback to primary bot for location resolution if needed
			if worker != p.primaryWorker && p.primaryWorker.Ready {
				location, _, _, err = p.primaryWorker.FetchFileLocation(ctx, chatID, msgID, false)
			}
			if err != nil {
				lastErr = err
				worker.MarkFailure()
				continue
			}
		}

		chunk, err := worker.DownloadChunk(ctx, location, offset, limit)
		if err == nil {
			worker.MarkSuccess()
			return chunk, nil
		}

		lastErr = err
		worker.MarkFailure()

		if errors.Is(err, ErrFileReferenceExpired) {
			log.Warnf("File reference expired for msg %d on worker #%d; refreshing...", msgID, worker.Index)
			worker.locationMu.Lock()
			delete(worker.locationCache, msgID)
			worker.locationMu.Unlock()

			freshLoc, _, _, refreshErr := worker.FetchFileLocation(ctx, chatID, msgID, true)
			if refreshErr != nil && worker != p.primaryWorker && p.primaryWorker.Ready {
				freshLoc, _, _, refreshErr = p.primaryWorker.FetchFileLocation(ctx, chatID, msgID, true)
			}
			if refreshErr == nil {
				chunk, retryErr := worker.DownloadChunk(ctx, freshLoc, offset, limit)
				if retryErr == nil {
					worker.MarkSuccess()
					return chunk, nil
				}
				lastErr = retryErr
			}
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("chunk download failed after %d attempts", maxAttempts)
}

// DownloadChunk downloads an audio chunk using an eligible worker with fallback.
func (p *Pool) DownloadChunk(ctx context.Context, location *tg.InputDocumentFileLocation, offset int64, limit int) ([]byte, error) {
	priority := models.PriorityFromContext(ctx)
	p.mu.RLock()
	workerCount := len(p.allWorkers)
	p.mu.RUnlock()

	maxAttempts := workerCount
	if maxAttempts < 2 {
		maxAttempts = 2
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		worker := p.AcquireWorkerWithPriority(priority)
		if worker == nil || !worker.Ready {
			return nil, ErrWorkerNotReady
		}

		chunk, err := worker.DownloadChunk(ctx, location, offset, limit)
		if err == nil {
			worker.MarkSuccess()
			return chunk, nil
		}
		lastErr = err
		worker.MarkFailure()
	}

	return nil, lastErr
}

// RegisterTrack registers or caches a track canonical record.
func (p *Pool) RegisterTrack(track *models.Track) {
	p.tracksMu.Lock()
	p.tracks[track.ID] = track
	p.tracksMu.Unlock()

	if p.db != nil {
		_ = p.db.SaveTrack(context.Background(), track)
	}
}

// ResolveTrack retrieves a track record from SQLite DB or in-memory cache.
func (p *Pool) ResolveTrack(ctx context.Context, trackID string) (*models.Track, error) {
	if p.db != nil {
		if t, err := p.db.GetTrack(ctx, trackID); err == nil && t != nil {
			if t.ChatID == 0 && p.config != nil && p.config.ChatID != 0 {
				t.ChatID = p.config.ChatID
			}
			return t, nil
		}
	}

	p.tracksMu.RLock()
	defer p.tracksMu.RUnlock()
	if t, ok := p.tracks[trackID]; ok {
		if t.ChatID == 0 && p.config != nil && p.config.ChatID != 0 {
			t.ChatID = p.config.ChatID
		}
		return t, nil
	}
	return nil, fmt.Errorf("track not found: %s", trackID)
}
