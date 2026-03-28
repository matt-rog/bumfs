package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// telegramResponse wraps a Telegram Bot API response.
type telegramResponse struct {
	OK     bool        `json:"ok"`
	Result interface{} `json:"result"`
}

// mockServer creates a test HTTP server that simulates the Telegram Bot API.
// It returns the server and a bot token string that routes requests to it.
// uploaded captures files sent via sendDocument keyed by filename.
func mockServer(t *testing.T, uploaded map[string][]byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/getMe"):
			json.NewEncoder(w).Encode(telegramResponse{
				OK: true,
				Result: map[string]interface{}{
					"id":         12345,
					"is_bot":     true,
					"first_name": "TestBot",
					"username":   "test_bot",
				},
			})

		case strings.HasSuffix(path, "/sendDocument"):
			err := r.ParseMultipartForm(10 << 20)
			if err != nil {
				t.Logf("sendDocument: parse form: %v", err)
				w.WriteHeader(500)
				return
			}

			file, header, err := r.FormFile("document")
			if err != nil {
				t.Logf("sendDocument: get file: %v", err)
				w.WriteHeader(500)
				return
			}
			defer file.Close()

			data, _ := io.ReadAll(file)
			filename := header.Filename
			uploaded[filename] = data

			// Return a fake file_id based on the filename
			fileID := "fileid_" + filename
			json.NewEncoder(w).Encode(telegramResponse{
				OK: true,
				Result: map[string]interface{}{
					"message_id": 1,
					"document": map[string]interface{}{
						"file_id":        fileID,
						"file_unique_id": "unique_" + filename,
						"file_name":      filename,
						"file_size":      len(data),
					},
				},
			})

		case strings.HasSuffix(path, "/getFile"):
			// The bot library sends params as multipart form data
			r.ParseMultipartForm(1 << 20)
			fileID := r.FormValue("file_id")

			// Extract chunk name from file_id
			name := strings.TrimPrefix(fileID, "fileid_")
			filePath := "documents/" + name

			json.NewEncoder(w).Encode(telegramResponse{
				OK: true,
				Result: map[string]interface{}{
					"file_id":        fileID,
					"file_unique_id": "unique_" + name,
					"file_path":      filePath,
					"file_size":      len(uploaded[name]),
				},
			})

		case strings.Contains(path, "/file/bot"):
			// Download endpoint: /file/bot<token>/documents/<name>
			idx := strings.Index(path, "/documents/")
			if idx < 0 {
				w.WriteHeader(404)
				return
			}
			name := path[idx+len("/documents/"):]
			data, ok := uploaded[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(data)

		default:
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"ok":false,"description":"not found: %s"}`, path)
		}
	}))
}

// newTestBackend creates a Backend wired to the mock server.
func newTestBackend(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "index.json")

	// The go-telegram/bot library uses the token to build URLs.
	// We need to override the server URL. The library constructs URLs as:
	//   https://api.telegram.org/bot<token>/method
	// We can't easily override this, so instead we'll build the backend
	// manually and set the bot's server URL.
	b, err := newWithServerURL(srv.URL, "fake-token", 12345, dbPath)
	if err != nil {
		t.Fatalf("create test backend: %v", err)
	}
	return b
}

func TestName(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	b := newTestBackend(t, srv)
	if b.Name() != "telegram" {
		t.Errorf("Name() = %q, want %q", b.Name(), "telegram")
	}
}

func TestWriteAndRead(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkData := []byte("hello, telegram chunk!")
	chunkID := "abc123hash"

	// Write
	if err := b.Write(ctx, chunkID, chunkData); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify it was uploaded
	if _, ok := uploaded[chunkID]; !ok {
		t.Fatal("chunk was not uploaded to mock server")
	}

	// Read back
	got, err := b.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != string(chunkData) {
		t.Errorf("Read = %q, want %q", got, chunkData)
	}
}

func TestReadNonExistent(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
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

func TestDeleteRemovesFromIndex(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkID := "deleteme"
	if err := b.Write(ctx, chunkID, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Delete
	if err := b.Delete(ctx, chunkID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Read should fail
	_, err := b.Read(ctx, chunkID)
	if err == nil {
		t.Fatal("Read after Delete: expected error, got nil")
	}
}

func TestHealthCheck(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	if err := b.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestCapacity(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
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

func TestIndexPersistence(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "index.json")

	// Create first backend, write a chunk
	b1, err := newWithServerURL(srv.URL, "fake-token", 12345, dbPath)
	if err != nil {
		t.Fatalf("create backend 1: %v", err)
	}

	ctx := context.Background()
	chunkID := "persist_test"
	chunkData := []byte("persistent data")

	if err := b1.Write(ctx, chunkID, chunkData); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify index file exists
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("index file not created: %v", err)
	}

	// Create second backend with same dbPath — should load existing index
	b2, err := newWithServerURL(srv.URL, "fake-token", 12345, dbPath)
	if err != nil {
		t.Fatalf("create backend 2: %v", err)
	}

	got, err := b2.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read from new backend: %v", err)
	}

	if string(got) != string(chunkData) {
		t.Errorf("Read = %q, want %q", got, chunkData)
	}
}

func TestWriteOverwrite(t *testing.T) {
	uploaded := make(map[string][]byte)
	srv := mockServer(t, uploaded)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ctx := context.Background()

	chunkID := "overwrite_test"

	// Write first version
	if err := b.Write(ctx, chunkID, []byte("version1")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}

	// Write second version (same ID, new data)
	if err := b.Write(ctx, chunkID, []byte("version2")); err != nil {
		t.Fatalf("Write v2: %v", err)
	}

	// Read should return the latest version
	got, err := b.Read(ctx, chunkID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != "version2" {
		t.Errorf("Read = %q, want %q", got, "version2")
	}
}
