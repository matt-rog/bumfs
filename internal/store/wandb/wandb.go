package wandb

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/matt-rog/bumfs/internal/config"
	"github.com/matt-rog/bumfs/internal/store"
)

func init() {
	store.Register("wandb", func(cfg config.BackendConfig, dataDir string) (store.StorageConnector, error) {
		dbPath := filepath.Join(dataDir, "wandb_index.json")
		return New(cfg.ApiKey, cfg.Entity, cfg.Project, dbPath)
	})
}

const apiBase = "https://api.wandb.ai"

// Backend stores chunks as W&B artifacts.
type Backend struct {
	apiKey  string
	entity  string
	project string
	client  *http.Client
	index   *store.Index[string]
	runMu   sync.Mutex
	runOK   bool
}

var _ store.StorageConnector = (*Backend)(nil)

// New creates a W&B storage backend.
func New(apiKey, entity, project, dbPath string) (*Backend, error) {
	idx, err := store.NewIndex[string](dbPath)
	if err != nil {
		return nil, fmt.Errorf("wandb backend: load index: %w", err)
	}
	return &Backend{
		apiKey:  apiKey,
		entity:  entity,
		project: project,
		client:  &http.Client{},
		index:   idx,
	}, nil
}

func (b *Backend) Name() string { return "wandb" }

func (b *Backend) Capacity() (total, used, free uint64) {
	total = 100 * 1024 * 1024 * 1024 // 100GB free tier
	count := uint64(b.index.Len())
	used = count * 1024 * 1024 // estimate 1MB per chunk
	if used > total {
		free = 0
	} else {
		free = total - used
	}
	return
}

func (b *Backend) HealthCheck(ctx context.Context) error {
	_, err := b.gql(ctx, `{ viewer { username } }`, nil)
	return err
}

func (b *Backend) Write(ctx context.Context, id string, data []byte) error {
	if err := b.ensureRun(ctx); err != nil {
		return fmt.Errorf("wandb write %s: %w", id, err)
	}

	fileMD5 := md5.Sum(data)
	fileMD5B64 := base64.StdEncoding.EncodeToString(fileMD5[:])

	// Artifact digest: hex(MD5("wandb-artifact-manifest-v1\nchunk.bin:<fileMD5B64>\n"))
	dh := md5.New()
	dh.Write([]byte("wandb-artifact-manifest-v1\n"))
	fmt.Fprintf(dh, "chunk.bin:%s\n", fileMD5B64)
	artifactDigest := hex.EncodeToString(dh.Sum(nil))

	// Step 1: CreateArtifact
	resp, err := b.gql(ctx,
		`mutation($input:CreateArtifactInput!){createArtifact(input:$input){artifact{id state}}}`,
		map[string]any{"input": map[string]any{
			"entityName": b.entity, "projectName": b.project,
			"artifactTypeName": "bumfs-chunk", "artifactCollectionName": "bumfs-" + id,
			"runName": "bumfs-store", "digest": artifactDigest, "digestAlgorithm": "MANIFEST_MD5",
		}})
	if err != nil {
		return fmt.Errorf("wandb write %s: create artifact: %w", id, err)
	}
	var ca struct {
		CreateArtifact struct {
			Artifact struct{ ID string `json:"id"` } `json:"artifact"`
		} `json:"createArtifact"`
	}
	if err := json.Unmarshal(resp, &ca); err != nil {
		return fmt.Errorf("wandb write %s: parse artifact: %w", id, err)
	}
	artID := ca.CreateArtifact.Artifact.ID
	artIntID := decodeIntID(artID)

	// Step 2: Build manifest
	manifest, _ := json.Marshal(map[string]any{
		"contents": map[string]any{
			"chunk.bin": map[string]any{"digest": fileMD5B64, "size": len(data)},
		},
		"storagePolicy": "wandb-storage-policy-v1", "storagePolicyConfig": map[string]any{},
		"version": 1,
	})
	mh := md5.Sum(manifest)
	manifestMD5B64 := base64.StdEncoding.EncodeToString(mh[:])

	// Step 3: CreateArtifactManifest
	_, err = b.gql(ctx,
		`mutation($input:CreateArtifactManifestInput!){createArtifactManifest(input:$input){artifactManifest{id}}}`,
		map[string]any{"input": map[string]any{
			"artifactID": artID, "entityName": b.entity, "projectName": b.project,
			"runName": "bumfs-store", "name": "wandb_manifest.json", "type": "FULL", "digest": manifestMD5B64,
		}})
	if err != nil {
		return fmt.Errorf("wandb write %s: create manifest: %w", id, err)
	}

	// Step 4: Get manifest upload URL via CreateRunFiles
	manifestPath := fmt.Sprintf("artifact/%s/wandb_manifest.json", artIntID)
	resp, err = b.gql(ctx,
		`mutation($input:CreateRunFilesInput!){createRunFiles(input:$input){files{id name uploadUrl}}}`,
		map[string]any{"input": map[string]any{
			"entityName": b.entity, "projectName": b.project,
			"runName": "bumfs-store", "files": []string{manifestPath},
		}})
	if err != nil {
		return fmt.Errorf("wandb write %s: run files for manifest: %w", id, err)
	}
	var rf struct {
		CreateRunFiles struct {
			Files []struct{ UploadUrl string `json:"uploadUrl"` } `json:"files"`
		} `json:"createRunFiles"`
	}
	if err := json.Unmarshal(resp, &rf); err != nil || len(rf.CreateRunFiles.Files) == 0 {
		return fmt.Errorf("wandb write %s: no manifest upload URL", id)
	}

	// Step 5: Upload manifest
	if err := b.upload(ctx, rf.CreateRunFiles.Files[0].UploadUrl, manifest, "application/json", ""); err != nil {
		return fmt.Errorf("wandb write %s: upload manifest: %w", id, err)
	}

	// Step 6: CreateArtifactFiles + upload
	resp, err = b.gql(ctx,
		`mutation($input:CreateArtifactFilesInput!){createArtifactFiles(input:$input){files{edges{node{uploadUrl uploadHeaders}}}}}`,
		map[string]any{"input": map[string]any{
			"artifactFiles": []map[string]any{{"artifactID": artID, "name": "chunk.bin", "md5": fileMD5B64}},
			"storageLayout": "V2",
		}})
	if err != nil {
		return fmt.Errorf("wandb write %s: create artifact files: %w", id, err)
	}
	var af struct {
		CreateArtifactFiles struct {
			Files struct {
				Edges []struct {
					Node struct {
						UploadUrl *string `json:"uploadUrl"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"files"`
		} `json:"createArtifactFiles"`
	}
	if err := json.Unmarshal(resp, &af); err != nil {
		return fmt.Errorf("wandb write %s: parse artifact files: %w", id, err)
	}
	if len(af.CreateArtifactFiles.Files.Edges) > 0 {
		if url := af.CreateArtifactFiles.Files.Edges[0].Node.UploadUrl; url != nil {
			if err := b.upload(ctx, *url, data, "application/octet-stream", fileMD5B64); err != nil {
				return fmt.Errorf("wandb write %s: upload file: %w", id, err)
			}
		}
		// nil uploadUrl = content already exists (dedup)
	}

	// Step 7: CommitArtifact
	_, err = b.gql(ctx,
		`mutation($input:CommitArtifactInput!){commitArtifact(input:$input){artifact{id state}}}`,
		map[string]any{"input": map[string]any{"artifactID": artID}})
	if err != nil {
		return fmt.Errorf("wandb write %s: commit: %w", id, err)
	}

	b.index.Set(id, artID)
	return b.index.Save()
}

func (b *Backend) Read(ctx context.Context, id string) ([]byte, error) {
	artID, ok := b.index.Get(id)
	if !ok {
		return nil, fmt.Errorf("wandb read %s: not in index", id)
	}

	resp, err := b.gql(ctx,
		fmt.Sprintf(`{artifact(id:%q){files{edges{node{name directUrl}}}}}`, artID), nil)
	if err != nil {
		return nil, fmt.Errorf("wandb read %s: query: %w", id, err)
	}
	var ar struct {
		Artifact struct {
			Files struct {
				Edges []struct {
					Node struct {
						Name      string `json:"name"`
						DirectUrl string `json:"directUrl"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"files"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(resp, &ar); err != nil {
		return nil, fmt.Errorf("wandb read %s: parse: %w", id, err)
	}

	var downloadURL string
	for _, e := range ar.Artifact.Files.Edges {
		if e.Node.Name == "chunk.bin" {
			downloadURL = e.Node.DirectUrl
			break
		}
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("wandb read %s: no download URL", id)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, err
	}
	hr, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wandb read %s: download: %w", id, err)
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wandb read %s: status %d", id, hr.StatusCode)
	}
	return io.ReadAll(hr.Body)
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	artID, ok := b.index.Get(id)
	if !ok {
		return nil
	}

	_, err := b.gql(ctx,
		`mutation($input:DeleteArtifactInput!){deleteArtifact(input:$input){artifact{id}}}`,
		map[string]any{"input": map[string]any{"artifactID": artID, "deleteAliases": true}})
	if err != nil {
		return fmt.Errorf("wandb delete %s: %w", id, err)
	}

	b.index.Delete(id)
	return b.index.Save()
}

// --- helpers ---

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

func (b *Backend) gql(ctx context.Context, query string, vars map[string]any) (json.RawMessage, error) {
	body, _ := json.Marshal(gqlReq{Query: query, Variables: vars})
	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var gr struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("wandb: decode: %w", err)
	}
	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("wandb: %s", gr.Errors[0].Message)
	}
	return gr.Data, nil
}

func (b *Backend) upload(ctx context.Context, url string, data []byte, contentType, md5B64 string) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if md5B64 != "" {
		req.Header.Set("Content-MD5", md5B64)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload status %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (b *Backend) ensureRun(ctx context.Context) error {
	b.runMu.Lock()
	defer b.runMu.Unlock()
	if b.runOK {
		return nil
	}
	_, err := b.gql(ctx,
		`mutation($input:UpsertBucketInput!){upsertBucket(input:$input){bucket{id}}}`,
		map[string]any{"input": map[string]any{
			"entityName": b.entity, "modelName": b.project, "name": "bumfs-store",
		}})
	if err != nil {
		return fmt.Errorf("ensure run: %w", err)
	}
	b.runOK = true
	return nil
}

func decodeIntID(gqlID string) string {
	decoded, err := base64.StdEncoding.DecodeString(gqlID)
	if err != nil {
		return gqlID
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) == 2 {
		return parts[len(parts)-1]
	}
	return gqlID
}

