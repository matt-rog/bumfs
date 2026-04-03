package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockState tracks assets across mock endpoints.
type mockState struct {
	mu        sync.Mutex
	assets    map[string][]byte // name → data
	assetIDs  map[string]int64  // name → asset ID
	nextID    int64
	releaseID int64
}

func newMockState() *mockState {
	return &mockState{
		assets:    make(map[string][]byte),
		assetIDs:  make(map[string]int64),
		nextID:    1000,
		releaseID: 42,
	}
}

// mockServer creates a test HTTP server simulating both api.github.com and uploads.github.com.
func mockServer(t *testing.T, state *mockState) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer fake-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}

		path := r.URL.Path

		switch {
		// GET /repos/{owner}/{repo} — health check
		case r.Method == http.MethodGet && path == "/repos/testowner/testrepo":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":1,"full_name":"testowner/testrepo"}`)

		// GET /repos/{owner}/{repo}/releases/tags/bumfs-storage — find release
		case r.Method == http.MethodGet && path == "/repos/testowner/testrepo/releases/tags/bumfs-storage":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": state.releaseID})

		// POST /repos/{owner}/{repo}/releases — create release
		case r.Method == http.MethodPost && path == "/repos/testowner/testrepo/releases":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": state.releaseID})

		// POST /repos/{owner}/{repo}/releases/{id}/assets?name={name} — upload asset
		case r.Method == http.MethodPost && strings.Contains(path, "/releases/42/assets"):
			name := r.URL.Query().Get("name")
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(500)
				return
			}

			state.mu.Lock()
			assetID := state.nextID
			state.nextID++
			state.assets[name] = data
			state.assetIDs[name] = assetID
			state.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": assetID, "name": name})

		// GET /repos/{owner}/{repo}/releases/assets/{id} — download asset
		case r.Method == http.MethodGet && strings.Contains(path, "/releases/assets/"):
			accept := r.Header.Get("Accept")
			// Parse asset ID from path
			parts := strings.Split(path, "/releases/assets/")
			if len(parts) != 2 {
				w.WriteHeader(404)
				return
			}

			var assetID int64
			fmt.Sscanf(parts[1], "%d", &assetID)

			state.mu.Lock()
			var found string
			var data []byte
			for name, id := range state.assetIDs {
				if id == assetID {
					found = name
					data = state.assets[name]
					break
				}
			}
			state.mu.Unlock()

			if found == "" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}

			if accept == "application/octet-stream" {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write(data)
			} else {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"id": assetID, "name": found})
			}

		// DELETE /repos/{owner}/{repo}/releases/assets/{id} — delete asset
		case r.Method == http.MethodDelete && strings.Contains(path, "/releases/assets/"):
			parts := strings.Split(path, "/releases/assets/")
			if len(parts) != 2 {
				w.WriteHeader(404)
				return
			}

			var assetID int64
			fmt.Sscanf(parts[1], "%d", &assetID)

			state.mu.Lock()
			for name, id := range state.assetIDs {
				if id == assetID {
					delete(state.assets, name)
					delete(state.assetIDs, name)
					break
				}
			}
			state.mu.Unlock()

			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"message":"not found: %s %s"}`, r.Method, path)
		}
	}))
}

func newTestBackend(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "index.json")
	b, err := newBackend("fake-token", "testowner", "testrepo", dbPath, srv.URL, srv.URL)
	if err != nil {
		t.Fatalf("create test backend: %v", err)
	}
	return b
}

func TestName(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	if b.Name() != "github" {
		t.Errorf("Name() = %q, want %q", b.Name(), "github")
	}
}

func TestWriteAndRead(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkData := []byte("hello, github chunk!")
	chunkID := "abc123hash"

	if err := b.Write(ctx, chunkID, chunkData); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify asset was stored
	state.mu.Lock()
	if _, ok := state.assets[chunkID]; !ok {
		t.Fatal("chunk was not uploaded to mock server")
	}
	state.mu.Unlock()

	got, err := b.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != string(chunkData) {
		t.Errorf("Read = %q, want %q", got, chunkData)
	}
}

func TestDeleteRemovesFromIndex(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkID := "deleteme"
	if err := b.Write(ctx, chunkID, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := b.Delete(ctx, chunkID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := b.Read(ctx, chunkID)
	if err == nil {
		t.Fatal("Read after Delete: expected error, got nil")
	}
}

func TestHealthCheck(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	if err := b.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestCapacity(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)

	total, used, free := b.Capacity()
	if total != math.MaxUint64 {
		t.Errorf("Capacity total = %d, want MaxUint64", total)
	}
	if used != 0 {
		t.Errorf("Capacity used = %d, want 0", used)
	}
	if free != math.MaxUint64 {
		t.Errorf("Capacity free = %d, want MaxUint64", free)
	}
}

func TestEnsureReleaseCreatesIfMissing(t *testing.T) {
	state := newMockState()

	// Server that returns 404 for tag lookup, then creates
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/releases/tags/bumfs-storage"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/releases"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": state.releaseID})

		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "index.json")
	b, err := newBackend("fake-token", "testowner", "testrepo", dbPath, srv.URL, srv.URL)
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}

	ctx := context.Background()
	id, err := b.ensureRelease(ctx)
	if err != nil {
		t.Fatalf("ensureRelease: %v", err)
	}
	if id != state.releaseID {
		t.Errorf("ensureRelease = %d, want %d", id, state.releaseID)
	}

	// Second call should use cached value
	id2, err := b.ensureRelease(ctx)
	if err != nil {
		t.Fatalf("ensureRelease (cached): %v", err)
	}
	if id2 != id {
		t.Errorf("cached releaseID = %d, want %d", id2, id)
	}
}

func TestReadNonExistent(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	_, err := b.Read(ctx, "nonexistent")
	if err == nil {
		t.Fatal("Read non-existent: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found in index") {
		t.Errorf("Read error = %q, want it to contain 'not found in index'", err)
	}
}

func TestWriteOverwrite(t *testing.T) {
	state := newMockState()
	srv := mockServer(t, state)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkID := "overwrite_test"

	if err := b.Write(ctx, chunkID, []byte("version1")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}

	if err := b.Write(ctx, chunkID, []byte("version2")); err != nil {
		t.Fatalf("Write v2: %v", err)
	}

	got, err := b.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != "version2" {
		t.Errorf("Read = %q, want %q", got, "version2")
	}
}
