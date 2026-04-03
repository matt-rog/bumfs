package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"

	"github.com/matt-rog/bumfs/internal/config"
	"github.com/matt-rog/bumfs/internal/store"
)

func init() {
	store.Register("github", func(cfg config.BackendConfig, dataDir string) (store.StorageConnector, error) {
		dbPath := filepath.Join(dataDir, "github_index.json")
		return New(cfg.Token, cfg.Owner, cfg.Repo, dbPath)
	})
}

// Backend stores chunks as GitHub release assets.
type Backend struct {
	token      string
	owner      string
	repo       string
	client     *http.Client
	index      *store.Index[int64]
	releaseID  int64  // cached release ID
	apiBase    string // "https://api.github.com" by default
	uploadBase string // "https://uploads.github.com" by default
}

var _ store.StorageConnector = (*Backend)(nil)

// New creates a GitHub Releases storage backend.
func New(token, owner, repo, dbPath string) (*Backend, error) {
	return newBackend(token, owner, repo, dbPath, "https://api.github.com", "https://uploads.github.com")
}

func newBackend(token, owner, repo, dbPath, apiBase, uploadBase string) (*Backend, error) {
	idx, err := store.NewIndex[int64](dbPath)
	if err != nil {
		return nil, fmt.Errorf("github backend: load index: %w", err)
	}
	return &Backend{
		token:      token,
		owner:      owner,
		repo:       repo,
		client:     &http.Client{},
		index:      idx,
		apiBase:    apiBase,
		uploadBase: uploadBase,
	}, nil
}

func (b *Backend) Name() string { return "github" }

func (b *Backend) Write(ctx context.Context, id string, data []byte) error {
	releaseID, err := b.ensureRelease(ctx)
	if err != nil {
		return fmt.Errorf("github write %s: %w", id, err)
	}

	// Delete existing asset if overwriting
	oldAssetID, exists := b.index.Get(id)
	if exists {
		b.deleteAsset(ctx, oldAssetID)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s",
		b.uploadBase, b.owner, b.repo, releaseID, id)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("github write %s: create request: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("github write %s: upload: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github write %s: status %d: %s", id, resp.StatusCode, body)
	}

	var asset struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&asset); err != nil {
		return fmt.Errorf("github write %s: decode response: %w", id, err)
	}

	b.index.Set(id, asset.ID)

	if err := b.index.Save(); err != nil {
		return fmt.Errorf("github write %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) Read(ctx context.Context, id string) ([]byte, error) {
	assetID, ok := b.index.Get(id)
	if !ok {
		return nil, fmt.Errorf("github read %s: chunk not found in index", id)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d",
		b.apiBase, b.owner, b.repo, assetID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("github read %s: create request: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github read %s: download: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github read %s: status %d: %s", id, resp.StatusCode, body)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github read %s: read body: %w", id, err)
	}

	return data, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	assetID, ok := b.index.Get(id)
	if ok {
		if err := b.deleteAsset(ctx, assetID); err != nil {
			return fmt.Errorf("github delete %s: %w", id, err)
		}
	}

	b.index.Delete(id)

	if err := b.index.Save(); err != nil {
		return fmt.Errorf("github delete %s: persist index: %w", id, err)
	}

	return nil
}

func (b *Backend) deleteAsset(ctx context.Context, assetID int64) error {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d",
		b.apiBase, b.owner, b.repo, assetID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete asset %d: status %d", assetID, resp.StatusCode)
	}
	return nil
}

func (b *Backend) Capacity() (total, used, free uint64) {
	n := uint64(b.index.Len())
	used = n * (1 << 20) // estimate: 1MB per chunk
	return math.MaxUint64, used, math.MaxUint64 - used
}

func (b *Backend) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("%s/repos/%s/%s", b.apiBase, b.owner, b.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("github healthcheck: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("github healthcheck: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github healthcheck: status %d", resp.StatusCode)
	}
	return nil
}

// ensureRelease finds or creates the bumfs-storage release.
func (b *Backend) ensureRelease(ctx context.Context) (int64, error) {
	if b.releaseID != 0 {
		return b.releaseID, nil
	}

	// Try to find existing release by tag
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/bumfs-storage",
		b.apiBase, b.owner, b.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("find release: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("find release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var rel struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
			return 0, fmt.Errorf("find release: decode: %w", err)
		}
		b.releaseID = rel.ID
		return b.releaseID, nil
	}

	// Create the release
	body, _ := json.Marshal(map[string]any{
		"tag_name": "bumfs-storage",
		"name":     "BumFS Storage",
		"body":     "Managed by BumFS. Do not delete.",
		"draft":    false,
	})

	createURL := fmt.Sprintf("%s/repos/%s/%s/releases", b.apiBase, b.owner, b.repo)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create release: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp2, err := b.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create release: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp2.Body)
		return 0, fmt.Errorf("create release: status %d: %s", resp2.StatusCode, respBody)
	}

	var rel struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&rel); err != nil {
		return 0, fmt.Errorf("create release: decode: %w", err)
	}
	b.releaseID = rel.ID
	return b.releaseID, nil
}

