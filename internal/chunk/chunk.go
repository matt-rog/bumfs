package chunk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/matt-rog/bumfs/internal/crypto"
	"github.com/matt-rog/bumfs/internal/store"
)

const DefaultChunkSize = 1 << 20 // 1 MB

// Ref identifies a chunk and its position within a file.
type Ref struct {
	ChunkID  string
	Index    int
	Size     int // plaintext size of this chunk
}

// Manager splits files into content-addressed encrypted chunks.
type Manager struct {
	chunkSize int
	enc       *crypto.Encryptor
	backend   store.StorageConnector
}

// NewManager creates a chunk manager.
func NewManager(chunkSize int, enc *crypto.Encryptor, backend store.StorageConnector) *Manager {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &Manager{
		chunkSize: chunkSize,
		enc:       enc,
		backend:   backend,
	}
}

// ChunkSize returns the configured chunk size.
func (m *Manager) ChunkSize() int {
	return m.chunkSize
}

// WriteFile splits data into chunks, encrypts, and stores them.
// Returns the list of chunk references.
func (m *Manager) WriteFile(ctx context.Context, data []byte) ([]Ref, error) {
	var refs []Ref
	for i := 0; i < len(data); i += m.chunkSize {
		end := i + m.chunkSize
		if end > len(data) {
			end = len(data)
		}
		plain := data[i:end]

		hash := sha256.Sum256(plain)
		chunkID := hex.EncodeToString(hash[:])

		ciphertext, err := m.enc.Encrypt(plain)
		if err != nil {
			return nil, fmt.Errorf("chunk write: encrypt chunk %d: %w", len(refs), err)
		}

		if err := m.backend.Write(ctx, chunkID, ciphertext); err != nil {
			return nil, fmt.Errorf("chunk write: store chunk %s: %w", chunkID, err)
		}

		refs = append(refs, Ref{
			ChunkID: chunkID,
			Index:   len(refs),
			Size:    len(plain),
		})
	}
	if len(refs) == 0 {
		// empty file, no chunks
		return nil, nil
	}
	return refs, nil
}

// ReadFile reads, decrypts, and reassembles a file from chunk references.
func (m *Manager) ReadFile(ctx context.Context, refs []Ref) ([]byte, error) {
	var data []byte
	for _, ref := range refs {
		ciphertext, err := m.backend.Read(ctx, ref.ChunkID)
		if err != nil {
			return nil, fmt.Errorf("chunk read: fetch %s: %w", ref.ChunkID, err)
		}
		plain, err := m.enc.Decrypt(ciphertext)
		if err != nil {
			return nil, fmt.Errorf("chunk read: decrypt %s: %w", ref.ChunkID, err)
		}
		data = append(data, plain...)
	}
	return data, nil
}

// DeleteChunks removes chunks from the backend.
func (m *Manager) DeleteChunks(ctx context.Context, refs []Ref) error {
	for _, ref := range refs {
		if err := m.backend.Delete(ctx, ref.ChunkID); err != nil {
			return fmt.Errorf("chunk delete %s: %w", ref.ChunkID, err)
		}
	}
	return nil
}
