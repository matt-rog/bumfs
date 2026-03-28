//go:build fuse3

package meta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesRoot(t *testing.T) {
	s := openTestStore(t)
	st, err := s.GetInode(1)
	if err != nil {
		t.Fatalf("GetInode(1): %v", err)
	}
	if st.Ino != 1 {
		t.Fatalf("root ino = %d, want 1", st.Ino)
	}
	if st.Mode != 0o040755 {
		t.Fatalf("root mode = %o, want 040755", st.Mode)
	}
	if st.Nlink < 2 {
		t.Fatalf("root nlink = %d, want >= 2", st.Nlink)
	}
}

func TestCreateAndLookupChild(t *testing.T) {
	s := openTestStore(t)
	ino, err := s.CreateNode(1, "hello.txt", 0o100644, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	st, err := s.LookupChild(1, "hello.txt")
	if err != nil {
		t.Fatalf("LookupChild: %v", err)
	}
	if st.Ino != ino {
		t.Fatalf("ino mismatch: got %d, want %d", st.Ino, ino)
	}
	if st.Name != "hello.txt" {
		t.Fatalf("name = %q, want hello.txt", st.Name)
	}
}

func TestCreateDuplicateFails(t *testing.T) {
	s := openTestStore(t)
	_, err := s.CreateNode(1, "dup", 0o100644, 0, 0)
	if err != nil {
		t.Fatalf("CreateNode 1: %v", err)
	}
	_, err = s.CreateNode(1, "dup", 0o100644, 0, 0)
	if err == nil {
		t.Fatal("expected UNIQUE constraint error")
	}
}

func TestLookupChildNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.LookupChild(1, "nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "inode not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLookupPathRoot(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"/", ""} {
		st, err := s.LookupPath(p)
		if err != nil {
			t.Fatalf("LookupPath(%q): %v", p, err)
		}
		if st.Ino != 1 {
			t.Fatalf("LookupPath(%q) ino = %d, want 1", p, st.Ino)
		}
	}
}

func TestLookupPathNested(t *testing.T) {
	s := openTestStore(t)
	aIno, err := s.CreateNode(1, "a", 0o040755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	bIno, err := s.CreateNode(aIno, "b", 0o040755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	cIno, err := s.CreateNode(bIno, "c", 0o100644, 0, 0)
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	st, err := s.LookupPath("/a/b/c")
	if err != nil {
		t.Fatalf("LookupPath: %v", err)
	}
	if st.Ino != cIno {
		t.Fatalf("ino = %d, want %d", st.Ino, cIno)
	}
}

func TestRemoveNode(t *testing.T) {
	s := openTestStore(t)
	_, err := s.CreateNode(1, "rmme", 0o100644, 0, 0)
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.RemoveNode(1, "rmme"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	_, err = s.LookupChild(1, "rmme")
	if err == nil {
		t.Fatal("expected not found after removal")
	}
}

func TestRemoveDirectoryAdjustsNlink(t *testing.T) {
	s := openTestStore(t)
	root, _ := s.GetInode(1)
	origNlink := root.Nlink

	_, err := s.CreateNode(1, "subdir", 0o040755, 0, 0)
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	root, _ = s.GetInode(1)
	if root.Nlink != origNlink+1 {
		t.Fatalf("nlink after mkdir = %d, want %d", root.Nlink, origNlink+1)
	}

	if err := s.RemoveNode(1, "subdir"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	root, _ = s.GetInode(1)
	if root.Nlink != origNlink {
		t.Fatalf("nlink after rmdir = %d, want %d", root.Nlink, origNlink)
	}
}

func TestListDir(t *testing.T) {
	s := openTestStore(t)
	// Empty root
	entries, err := s.ListDir(1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}

	// Add children
	s.CreateNode(1, "a", 0o100644, 0, 0)
	s.CreateNode(1, "b", 0o040755, 0, 0)
	s.CreateNode(1, "c", 0o100644, 0, 0)

	entries, err = s.ListDir(1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, n := range []string{"a", "b", "c"} {
		if !names[n] {
			t.Fatalf("missing entry %q", n)
		}
	}
}

func TestSetAndGetFileChunks(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "chunked", 0o100644, 0, 0)

	chunks := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{
		{"aaa", 100, "local"},
		{"bbb", 100, "local"},
		{"ccc", 50, "local"},
	}
	if err := s.SetFileChunks(ino, chunks); err != nil {
		t.Fatalf("SetFileChunks: %v", err)
	}
	got, err := s.GetFileChunks(ino)
	if err != nil {
		t.Fatalf("GetFileChunks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(got))
	}
	for i, fc := range got {
		if fc.ChunkIdx != i {
			t.Fatalf("chunk %d: idx = %d", i, fc.ChunkIdx)
		}
		if fc.ChunkID != chunks[i].ChunkID {
			t.Fatalf("chunk %d: id = %s, want %s", i, fc.ChunkID, chunks[i].ChunkID)
		}
	}
}

func TestSetFileChunksReplacesOld(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "replace", 0o100644, 0, 0)

	first := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{{"old1", 100, "local"}, {"old2", 100, "local"}}
	s.SetFileChunks(ino, first)

	second := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{{"new1", 200, "local"}}
	s.SetFileChunks(ino, second)

	got, _ := s.GetFileChunks(ino)
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk after replace, got %d", len(got))
	}
	if got[0].ChunkID != "new1" {
		t.Fatalf("chunk id = %s, want new1", got[0].ChunkID)
	}
}

func TestRefCounting(t *testing.T) {
	s := openTestStore(t)
	ino1, _ := s.CreateNode(1, "f1", 0o100644, 0, 0)
	ino2, _ := s.CreateNode(1, "f2", 0o100644, 0, 0)

	shared := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{{"shared_chunk", 100, "local"}}

	s.SetFileChunks(ino1, shared)
	s.SetFileChunks(ino2, shared)

	// Both reference shared_chunk; unreferenced should be empty
	unreferenced, err := s.GetUnreferencedChunks()
	if err != nil {
		t.Fatalf("GetUnreferencedChunks: %v", err)
	}
	if len(unreferenced) != 0 {
		t.Fatalf("expected 0 unreferenced, got %d", len(unreferenced))
	}
}

func TestGetUnreferencedChunks(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "f", 0o100644, 0, 0)

	old := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{{"chunk_v1", 100, "local"}}
	s.SetFileChunks(ino, old)

	// Replace with new chunk — old chunk should become unreferenced
	newc := []struct {
		ChunkID string
		Size    int64
		Backend string
	}{{"chunk_v2", 200, "local"}}
	s.SetFileChunks(ino, newc)

	unreferenced, err := s.GetUnreferencedChunks()
	if err != nil {
		t.Fatalf("GetUnreferencedChunks: %v", err)
	}
	if len(unreferenced) != 1 || unreferenced[0] != "chunk_v1" {
		t.Fatalf("expected [chunk_v1], got %v", unreferenced)
	}
}

func TestUpdateSize(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "sized", 0o100644, 0, 0)
	s.UpdateSize(ino, 42)
	st, _ := s.GetInode(ino)
	if st.Size != 42 {
		t.Fatalf("size = %d, want 42", st.Size)
	}
}

func TestChmod(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "chm", 0o100644, 0, 0)
	s.Chmod(ino, 0o100600)
	st, _ := s.GetInode(ino)
	if st.Mode != 0o100600 {
		t.Fatalf("mode = %o, want 100600", st.Mode)
	}
}

func TestChown(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "cho", 0o100644, 0, 0)
	s.Chown(ino, 1000, 1000)
	st, _ := s.GetInode(ino)
	if st.Uid != 1000 || st.Gid != 1000 {
		t.Fatalf("uid=%d gid=%d, want 1000/1000", st.Uid, st.Gid)
	}
}

func TestUpdateTimes(t *testing.T) {
	s := openTestStore(t)
	ino, _ := s.CreateNode(1, "timed", 0o100644, 0, 0)
	s.UpdateTimes(ino, 100, 200, 300, 400)
	st, _ := s.GetInode(ino)
	if st.AtimeSec != 100 || st.AtimeNsec != 200 {
		t.Fatalf("atime = %d.%d", st.AtimeSec, st.AtimeNsec)
	}
	if st.MtimeSec != 300 || st.MtimeNsec != 400 {
		t.Fatalf("mtime = %d.%d", st.MtimeSec, st.MtimeNsec)
	}
}

func TestRenameBasic(t *testing.T) {
	s := openTestStore(t)
	s.CreateNode(1, "old", 0o100644, 0, 0)
	if err := s.Rename(1, "old", 1, "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	_, err := s.LookupChild(1, "old")
	if err == nil {
		t.Fatal("old name should be gone")
	}
	st, err := s.LookupChild(1, "new")
	if err != nil {
		t.Fatalf("LookupChild new: %v", err)
	}
	if st.Name != "new" {
		t.Fatalf("name = %q", st.Name)
	}
}

func TestRenameMoveToSubdir(t *testing.T) {
	s := openTestStore(t)
	dirIno, _ := s.CreateNode(1, "sub", 0o040755, 0, 0)
	s.CreateNode(1, "moveme", 0o100644, 0, 0)
	if err := s.Rename(1, "moveme", dirIno, "moveme"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	_, err := s.LookupChild(1, "moveme")
	if err == nil {
		t.Fatal("old location should be gone")
	}
	_, err = s.LookupChild(dirIno, "moveme")
	if err != nil {
		t.Fatalf("not found in new location: %v", err)
	}
}

func TestRenameOverwriteExisting(t *testing.T) {
	s := openTestStore(t)
	s.CreateNode(1, "src", 0o100644, 0, 0)
	s.CreateNode(1, "dst", 0o100644, 0, 0)
	if err := s.Rename(1, "src", 1, "dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	_, err := s.LookupChild(1, "src")
	if err == nil {
		t.Fatal("src should be gone")
	}
	_, err = s.LookupChild(1, "dst")
	if err != nil {
		t.Fatalf("dst should exist: %v", err)
	}
}

func TestRenameDirNlinkAdjustment(t *testing.T) {
	s := openTestStore(t)
	dir1, _ := s.CreateNode(1, "d1", 0o040755, 0, 0)
	dir2, _ := s.CreateNode(1, "d2", 0o040755, 0, 0)
	s.CreateNode(dir1, "child", 0o040755, 0, 0)

	d1Before, _ := s.GetInode(dir1)
	d2Before, _ := s.GetInode(dir2)

	s.Rename(dir1, "child", dir2, "child")

	d1After, _ := s.GetInode(dir1)
	d2After, _ := s.GetInode(dir2)

	if d1After.Nlink != d1Before.Nlink-1 {
		t.Fatalf("d1 nlink: %d -> %d (expected -1)", d1Before.Nlink, d1After.Nlink)
	}
	if d2After.Nlink != d2Before.Nlink+1 {
		t.Fatalf("d2 nlink: %d -> %d (expected +1)", d2Before.Nlink, d2After.Nlink)
	}
}

func TestCloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ino, err := s.CreateNode(1, "persist", 0o100644, 0, 0)
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer s2.Close()

	st, err := s2.LookupChild(1, "persist")
	if err != nil {
		t.Fatalf("LookupChild after reopen: %v", err)
	}
	if st.Ino != ino {
		t.Fatalf("ino mismatch after reopen: got %d, want %d", st.Ino, ino)
	}
}
