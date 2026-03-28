package meta

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// InodeStat holds metadata for a single inode.
type InodeStat struct {
	Ino       uint64
	ParentIno uint64
	Name      string
	Mode      uint32
	Uid       uint32
	Gid       uint32
	Size      int64
	Nlink     int64
	AtimeSec  int64
	AtimeNsec int64
	MtimeSec  int64
	MtimeNsec int64
	CtimeSec  int64
	CtimeNsec int64
}

// ChunkInfo describes a chunk stored in a backend.
type ChunkInfo struct {
	ChunkID string
	Size    int64
	Backend string
}

// FileChunk maps a chunk to its position within a file.
type FileChunk struct {
	Ino      uint64
	ChunkIdx int
	ChunkID  string
}

// Store is the SQLite-backed metadata store.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS inodes (
    ino        INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_ino INTEGER REFERENCES inodes(ino),
    name       TEXT NOT NULL,
    mode       INTEGER NOT NULL,
    uid        INTEGER NOT NULL,
    gid        INTEGER NOT NULL,
    size       INTEGER DEFAULT 0,
    nlink      INTEGER DEFAULT 1,
    atime_sec  INTEGER, atime_nsec INTEGER,
    mtime_sec  INTEGER, mtime_nsec INTEGER,
    ctime_sec  INTEGER, ctime_nsec INTEGER,
    UNIQUE(parent_ino, name)
);

CREATE TABLE IF NOT EXISTS chunks (
    chunk_id   TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    ref_count  INTEGER NOT NULL DEFAULT 1,
    backend    TEXT NOT NULL DEFAULT 'local'
);

CREATE TABLE IF NOT EXISTS file_chunks (
    ino        INTEGER REFERENCES inodes(ino) ON DELETE CASCADE,
    chunk_idx  INTEGER NOT NULL,
    chunk_id   TEXT REFERENCES chunks(chunk_id),
    PRIMARY KEY (ino, chunk_idx)
);
`

// Open opens (or creates) the metadata database.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("meta: mkdir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("meta: open db: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("meta: create schema: %w", err)
	}

	s := &Store{db: db}
	if err := s.ensureRoot(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) ensureRoot() error {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM inodes WHERE ino = 1").Scan(&count)
	if err != nil {
		return fmt.Errorf("meta: check root: %w", err)
	}
	if count > 0 {
		return nil
	}
	now := time.Now()
	_, err = s.db.Exec(`INSERT INTO inodes (ino, parent_ino, name, mode, uid, gid, size, nlink,
		atime_sec, atime_nsec, mtime_sec, mtime_nsec, ctime_sec, ctime_nsec)
		VALUES (1, 1, '', ?, ?, ?, 0, 2, ?, ?, ?, ?, ?, ?)`,
		0o040755, os.Getuid(), os.Getgid(),
		now.Unix(), int64(now.Nanosecond()),
		now.Unix(), int64(now.Nanosecond()),
		now.Unix(), int64(now.Nanosecond()))
	if err != nil {
		return fmt.Errorf("meta: create root inode: %w", err)
	}
	return nil
}

// LookupChild finds a child inode by parent and name.
func (s *Store) LookupChild(parentIno uint64, name string) (*InodeStat, error) {
	row := s.db.QueryRow(`SELECT ino, parent_ino, name, mode, uid, gid, size, nlink,
		atime_sec, atime_nsec, mtime_sec, mtime_nsec, ctime_sec, ctime_nsec
		FROM inodes WHERE parent_ino = ? AND name = ?`, parentIno, name)
	return scanInode(row)
}

// GetInode returns metadata for a single inode.
func (s *Store) GetInode(ino uint64) (*InodeStat, error) {
	row := s.db.QueryRow(`SELECT ino, parent_ino, name, mode, uid, gid, size, nlink,
		atime_sec, atime_nsec, mtime_sec, mtime_nsec, ctime_sec, ctime_nsec
		FROM inodes WHERE ino = ?`, ino)
	return scanInode(row)
}

// LookupPath resolves a path like "/foo/bar" to an inode.
func (s *Store) LookupPath(path string) (*InodeStat, error) {
	if path == "/" || path == "" {
		return s.GetInode(1)
	}

	// strip leading slash and split
	if path[0] == '/' {
		path = path[1:]
	}
	parts := splitPath(path)

	currentIno := uint64(1)
	var stat *InodeStat
	var err error
	for _, part := range parts {
		stat, err = s.LookupChild(currentIno, part)
		if err != nil {
			return nil, err
		}
		currentIno = stat.Ino
	}
	return stat, nil
}

// CreateNode creates a new inode (file or directory).
func (s *Store) CreateNode(parentIno uint64, name string, mode uint32, uid, gid uint32) (uint64, error) {
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO inodes (parent_ino, name, mode, uid, gid, size, nlink,
		atime_sec, atime_nsec, mtime_sec, mtime_nsec, ctime_sec, ctime_nsec)
		VALUES (?, ?, ?, ?, ?, 0, 1, ?, ?, ?, ?, ?, ?)`,
		parentIno, name, mode, uid, gid,
		now.Unix(), int64(now.Nanosecond()),
		now.Unix(), int64(now.Nanosecond()),
		now.Unix(), int64(now.Nanosecond()))
	if err != nil {
		return 0, fmt.Errorf("meta: create node: %w", err)
	}
	ino, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("meta: last insert id: %w", err)
	}

	// If creating a directory, bump parent's nlink
	if mode&0o170000 == 0o040000 {
		if _, err := s.db.Exec("UPDATE inodes SET nlink = nlink + 1 WHERE ino = ?", parentIno); err != nil {
			return 0, fmt.Errorf("meta: bump parent nlink: %w", err)
		}
	}

	return uint64(ino), nil
}

// RemoveNode removes an inode by parent and name.
func (s *Store) RemoveNode(parentIno uint64, name string) error {
	stat, err := s.LookupChild(parentIno, name)
	if err != nil {
		return err
	}

	// Delete file_chunks referencing this inode
	if _, err := s.db.Exec("DELETE FROM file_chunks WHERE ino = ?", stat.Ino); err != nil {
		return fmt.Errorf("meta: delete file_chunks: %w", err)
	}

	if _, err := s.db.Exec("DELETE FROM inodes WHERE ino = ?", stat.Ino); err != nil {
		return fmt.Errorf("meta: remove node: %w", err)
	}

	// If removing a directory, decrement parent's nlink
	if stat.Mode&0o170000 == 0o040000 {
		if _, err := s.db.Exec("UPDATE inodes SET nlink = nlink - 1 WHERE ino = ?", parentIno); err != nil {
			return fmt.Errorf("meta: dec parent nlink: %w", err)
		}
	}
	return nil
}

// ListDir returns all children of a directory inode.
func (s *Store) ListDir(parentIno uint64) ([]InodeStat, error) {
	rows, err := s.db.Query(`SELECT ino, parent_ino, name, mode, uid, gid, size, nlink,
		atime_sec, atime_nsec, mtime_sec, mtime_nsec, ctime_sec, ctime_nsec
		FROM inodes WHERE parent_ino = ? AND ino != ?`, parentIno, parentIno)
	if err != nil {
		return nil, fmt.Errorf("meta: list dir: %w", err)
	}
	defer rows.Close()

	var entries []InodeStat
	for rows.Next() {
		var st InodeStat
		err := rows.Scan(&st.Ino, &st.ParentIno, &st.Name, &st.Mode, &st.Uid, &st.Gid,
			&st.Size, &st.Nlink, &st.AtimeSec, &st.AtimeNsec,
			&st.MtimeSec, &st.MtimeNsec, &st.CtimeSec, &st.CtimeNsec)
		if err != nil {
			return nil, fmt.Errorf("meta: scan dir entry: %w", err)
		}
		entries = append(entries, st)
	}
	return entries, rows.Err()
}

// GetFileChunks returns the chunk references for a file, ordered by index.
func (s *Store) GetFileChunks(ino uint64) ([]FileChunk, error) {
	rows, err := s.db.Query(`SELECT ino, chunk_idx, chunk_id FROM file_chunks
		WHERE ino = ? ORDER BY chunk_idx`, ino)
	if err != nil {
		return nil, fmt.Errorf("meta: get file chunks: %w", err)
	}
	defer rows.Close()

	var chunks []FileChunk
	for rows.Next() {
		var fc FileChunk
		if err := rows.Scan(&fc.Ino, &fc.ChunkIdx, &fc.ChunkID); err != nil {
			return nil, fmt.Errorf("meta: scan file chunk: %w", err)
		}
		chunks = append(chunks, fc)
	}
	return chunks, rows.Err()
}

// SetFileChunks replaces the chunk mappings for a file.
// It also manages reference counts in the chunks table.
func (s *Store) SetFileChunks(ino uint64, chunks []struct {
	ChunkID string
	Size    int64
	Backend string
}) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("meta: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Decrement ref counts for old chunks
	oldChunks, err := s.GetFileChunks(ino)
	if err != nil {
		return err
	}
	for _, oc := range oldChunks {
		if _, err := tx.Exec("UPDATE chunks SET ref_count = ref_count - 1 WHERE chunk_id = ?", oc.ChunkID); err != nil {
			return fmt.Errorf("meta: dec ref count: %w", err)
		}
	}

	// Remove old mappings
	if _, err := tx.Exec("DELETE FROM file_chunks WHERE ino = ?", ino); err != nil {
		return fmt.Errorf("meta: clear file_chunks: %w", err)
	}

	// Insert new chunk records and mappings
	for i, c := range chunks {
		// Upsert into chunks table
		_, err := tx.Exec(`INSERT INTO chunks (chunk_id, size, backend, ref_count) VALUES (?, ?, ?, 1)
			ON CONFLICT(chunk_id) DO UPDATE SET ref_count = ref_count + 1`,
			c.ChunkID, c.Size, c.Backend)
		if err != nil {
			return fmt.Errorf("meta: upsert chunk: %w", err)
		}

		if _, err := tx.Exec("INSERT INTO file_chunks (ino, chunk_idx, chunk_id) VALUES (?, ?, ?)",
			ino, i, c.ChunkID); err != nil {
			return fmt.Errorf("meta: insert file_chunk: %w", err)
		}
	}

	return tx.Commit()
}

// UpdateSize updates the file size for an inode.
func (s *Store) UpdateSize(ino uint64, size int64) error {
	now := time.Now()
	_, err := s.db.Exec("UPDATE inodes SET size = ?, mtime_sec = ?, mtime_nsec = ? WHERE ino = ?",
		size, now.Unix(), int64(now.Nanosecond()), ino)
	return err
}

// UpdateTimes updates atime and mtime for an inode.
func (s *Store) UpdateTimes(ino uint64, atimeSec, atimeNsec, mtimeSec, mtimeNsec int64) error {
	_, err := s.db.Exec(`UPDATE inodes SET atime_sec = ?, atime_nsec = ?, mtime_sec = ?, mtime_nsec = ?,
		ctime_sec = ?, ctime_nsec = ? WHERE ino = ?`,
		atimeSec, atimeNsec, mtimeSec, mtimeNsec,
		time.Now().Unix(), int64(time.Now().Nanosecond()), ino)
	return err
}

// Chmod updates mode for an inode.
func (s *Store) Chmod(ino uint64, mode uint32) error {
	now := time.Now()
	_, err := s.db.Exec("UPDATE inodes SET mode = ?, ctime_sec = ?, ctime_nsec = ? WHERE ino = ?",
		mode, now.Unix(), int64(now.Nanosecond()), ino)
	return err
}

// Chown updates uid and gid for an inode.
func (s *Store) Chown(ino uint64, uid, gid uint32) error {
	now := time.Now()
	_, err := s.db.Exec("UPDATE inodes SET uid = ?, gid = ?, ctime_sec = ?, ctime_nsec = ? WHERE ino = ?",
		uid, gid, now.Unix(), int64(now.Nanosecond()), ino)
	return err
}

// Rename moves an inode from one parent/name to another.
func (s *Store) Rename(oldParent uint64, oldName string, newParent uint64, newName string) error {
	// Check if target already exists and remove it
	existing, err := s.LookupChild(newParent, newName)
	if err == nil && existing != nil {
		if err := s.RemoveNode(newParent, newName); err != nil {
			return fmt.Errorf("meta: rename remove existing: %w", err)
		}
	}

	stat, err := s.LookupChild(oldParent, oldName)
	if err != nil {
		return fmt.Errorf("meta: rename lookup: %w", err)
	}

	now := time.Now()
	_, err = s.db.Exec("UPDATE inodes SET parent_ino = ?, name = ?, ctime_sec = ?, ctime_nsec = ? WHERE ino = ?",
		newParent, newName, now.Unix(), int64(now.Nanosecond()), stat.Ino)
	if err != nil {
		return fmt.Errorf("meta: rename: %w", err)
	}

	// Adjust nlinks if moving a directory between parents
	if stat.Mode&0o170000 == 0o040000 && oldParent != newParent {
		if _, err := s.db.Exec("UPDATE inodes SET nlink = nlink - 1 WHERE ino = ?", oldParent); err != nil {
			return err
		}
		if _, err := s.db.Exec("UPDATE inodes SET nlink = nlink + 1 WHERE ino = ?", newParent); err != nil {
			return err
		}
	}
	return nil
}

// GetUnreferencedChunks returns chunk IDs with ref_count <= 0.
func (s *Store) GetUnreferencedChunks() ([]string, error) {
	rows, err := s.db.Query("SELECT chunk_id FROM chunks WHERE ref_count <= 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteChunkRecord removes a chunk record from the metadata.
func (s *Store) DeleteChunkRecord(chunkID string) error {
	_, err := s.db.Exec("DELETE FROM chunks WHERE chunk_id = ?", chunkID)
	return err
}

func scanInode(row *sql.Row) (*InodeStat, error) {
	var st InodeStat
	err := row.Scan(&st.Ino, &st.ParentIno, &st.Name, &st.Mode, &st.Uid, &st.Gid,
		&st.Size, &st.Nlink, &st.AtimeSec, &st.AtimeNsec,
		&st.MtimeSec, &st.MtimeNsec, &st.CtimeSec, &st.CtimeNsec)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("meta: inode not found")
		}
		return nil, fmt.Errorf("meta: scan inode: %w", err)
	}
	return &st, nil
}

func splitPath(path string) []string {
	var parts []string
	current := ""
	for _, c := range path {
		if c == '/' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
