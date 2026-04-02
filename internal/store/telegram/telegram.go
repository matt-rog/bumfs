package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/matt-rog/bumfs/internal/config"
	"github.com/matt-rog/bumfs/internal/store"
)

func init() {
	store.Register("telegram", func(cfg config.BackendConfig, dataDir string) (store.StorageConnector, error) {
		dbPath := filepath.Join(dataDir, "telegram_index.json")
		return New(cfg.BotToken, cfg.ChatID, dbPath)
	})
}

// Backend stores chunks as Telegram documents in a chat.
type Backend struct {
	bot    *bot.Bot
	chatID int64
	index  *store.Index[string]
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

	idx, err := store.NewIndex[string](dbPath)
	if err != nil {
		return nil, fmt.Errorf("telegram backend: load index: %w", err)
	}

	return &Backend{
		bot:    b,
		chatID: chatID,
		index:  idx,
	}, nil
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
	b.index.Set(id, fileID)

	if err := b.index.Save(); err != nil {
		return fmt.Errorf("telegram write %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) Read(ctx context.Context, id string) ([]byte, error) {
	fileID, ok := b.index.Get(id)
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
	b.index.Delete(id)

	if err := b.index.Save(); err != nil {
		return fmt.Errorf("telegram delete %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) Capacity() (total, used, free uint64) {
	n := uint64(b.index.Len())
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

