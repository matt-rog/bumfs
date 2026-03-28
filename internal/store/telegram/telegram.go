package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/matt-rog/bumfs/internal/store"
)

// Backend stores chunks as Telegram documents in a chat.
type Backend struct {
	bot    *bot.Bot
	chatID int64
	mu     sync.RWMutex
	index  map[string]string // chunkID → telegram file_id
	dbPath string            // path to persist the index
}

var _ store.StorageConnector = (*Backend)(nil)

// New creates a Telegram storage backend.
// botToken is the Telegram Bot API token, chatID is the target chat,
// and dbPath is where the chunk→file_id index is persisted as JSON.
func New(botToken string, chatID int64, dbPath string) (*Backend, error) {
	return newWithOpts(botToken, chatID, dbPath)
}

// newWithServerURL creates a backend pointing at a custom API server (for testing).
func newWithServerURL(serverURL, botToken string, chatID int64, dbPath string) (*Backend, error) {
	return newWithOpts(botToken, chatID, dbPath, bot.WithServerURL(serverURL))
}

func newWithOpts(botToken string, chatID int64, dbPath string, opts ...bot.Option) (*Backend, error) {
	allOpts := append([]bot.Option{bot.WithSkipGetMe()}, opts...)
	b, err := bot.New(botToken, allOpts...)
	if err != nil {
		return nil, fmt.Errorf("telegram backend: create bot: %w", err)
	}

	backend := &Backend{
		bot:    b,
		chatID: chatID,
		index:  make(map[string]string),
		dbPath: dbPath,
	}

	if err := backend.loadIndex(); err != nil {
		return nil, fmt.Errorf("telegram backend: load index: %w", err)
	}

	return backend, nil
}

func (b *Backend) Name() string { return "telegram" }

func (b *Backend) Write(ctx context.Context, id string, data []byte) error {
	msg, err := b.bot.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID: b.chatID,
		Document: &models.InputFileUpload{
			Filename: id,
			Data:     bytes.NewReader(data),
		},
	})
	if err != nil {
		return fmt.Errorf("telegram write %s: %w", id, err)
	}

	fileID := msg.Document.FileID

	b.mu.Lock()
	b.index[id] = fileID
	b.mu.Unlock()

	if err := b.saveIndex(); err != nil {
		return fmt.Errorf("telegram write %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) Read(ctx context.Context, id string) ([]byte, error) {
	b.mu.RLock()
	fileID, ok := b.index[id]
	b.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("telegram read %s: chunk not found in index", id)
	}

	file, err := b.bot.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("telegram read %s: get file: %w", id, err)
	}

	downloadURL := b.bot.FileDownloadLink(file)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram read %s: create request: %w", id, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram read %s: download: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram read %s: download status %d", id, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram read %s: read body: %w", id, err)
	}

	return data, nil
}

func (b *Backend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	delete(b.index, id)
	b.mu.Unlock()

	if err := b.saveIndex(); err != nil {
		return fmt.Errorf("telegram delete %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) Capacity() (total, used, free uint64) {
	b.mu.RLock()
	n := uint64(len(b.index))
	b.mu.RUnlock()
	used = n * (1 << 20) // estimate: 1MB per chunk
	return math.MaxUint64, used, math.MaxUint64 - used
}

func (b *Backend) HealthCheck(ctx context.Context) error {
	_, err := b.bot.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram healthcheck: %w", err)
	}
	return nil
}

func (b *Backend) loadIndex() error {
	data, err := os.ReadFile(b.dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return json.Unmarshal(data, &b.index)
}

func (b *Backend) saveIndex() error {
	b.mu.RLock()
	data, err := json.Marshal(b.index)
	b.mu.RUnlock()

	if err != nil {
		return err
	}

	return os.WriteFile(b.dbPath, data, 0600)
}
