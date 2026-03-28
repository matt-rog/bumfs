//go:build fuse3

package fs

import (
	"bytes"
	"math"
	"path/filepath"
	"testing"

	"github.com/matt-rog/bumfs/internal/chunk"
	"github.com/matt-rog/bumfs/internal/crypto"
	"github.com/matt-rog/bumfs/internal/meta"
	"github.com/matt-rog/bumfs/internal/store/local"
	"github.com/winfsp/cgofuse/fuse"
)

func setupBumFS(t *testing.T) *BumFS {
	t.Helper()
	dir := t.TempDir()

	metaStore, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { metaStore.Close() })

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	enc, err := crypto.NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	backend, err := local.New(filepath.Join(dir, "chunks"))
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	chunkMgr := chunk.NewManager(0, enc, backend)
	return New(metaStore, chunkMgr, backend)
}

func TestGetattrRoot(t *testing.T) {
	fs := setupBumFS(t)
	var stat fuse.Stat_t
	errn := fs.Getattr("/", &stat, math.MaxUint64)
	if errn != 0 {
		t.Fatalf("Getattr root: errno %d", errn)
	}
	if stat.Mode != 0o040755 {
		t.Fatalf("root mode = %o, want 040755", stat.Mode)
	}
}

func TestMkdirAndGetattr(t *testing.T) {
	fs := setupBumFS(t)
	errn := fs.Mkdir("/mydir", 0o755)
	if errn != 0 {
		t.Fatalf("Mkdir: errno %d", errn)
	}
	var stat fuse.Stat_t
	errn = fs.Getattr("/mydir", &stat, math.MaxUint64)
	if errn != 0 {
		t.Fatalf("Getattr: errno %d", errn)
	}
	if stat.Mode&0o170000 != 0o040000 {
		t.Fatalf("not a directory: mode %o", stat.Mode)
	}
}

func TestMkdirDuplicate(t *testing.T) {
	fs := setupBumFS(t)
	fs.Mkdir("/dup", 0o755)
	errn := fs.Mkdir("/dup", 0o755)
	// SQLite UNIQUE constraint error maps to EIO via errno() since
	// the error doesn't satisfy os.IsExist()
	if errn != -fuse.EIO {
		t.Fatalf("expected EIO (-5) for duplicate mkdir, got %d", errn)
	}
}

func TestRmdirEmpty(t *testing.T) {
	fs := setupBumFS(t)
	fs.Mkdir("/empty", 0o755)
	errn := fs.Rmdir("/empty")
	if errn != 0 {
		t.Fatalf("Rmdir: errno %d", errn)
	}
	var stat fuse.Stat_t
	errn = fs.Getattr("/empty", &stat, math.MaxUint64)
	if errn != -fuse.ENOENT {
		t.Fatalf("expected ENOENT after rmdir, got %d", errn)
	}
}

func TestRmdirNonEmpty(t *testing.T) {
	fs := setupBumFS(t)
	fs.Mkdir("/notempty", 0o755)
	fs.Create("/notempty/file", 0, 0o644)
	errn := fs.Rmdir("/notempty")
	if errn != -fuse.ENOTEMPTY {
		t.Fatalf("expected ENOTEMPTY, got %d", errn)
	}
}

func TestCreateOpenRelease(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/newfile", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: errno %d", errn)
	}
	if fh == math.MaxUint64 {
		t.Fatal("got invalid file handle")
	}
	errn = fs.Release("/newfile", fh)
	if errn != 0 {
		t.Fatalf("Release: errno %d", errn)
	}
}

func TestWriteAndRead(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/wfile", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	data := []byte("hello bumfs")
	n := fs.Write("/wfile", data, 0, fh)
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}
	// Flush to persist
	fs.Flush("/wfile", fh)

	// Read back
	buf := make([]byte, 64)
	n = fs.Read("/wfile", buf, 0, fh)
	if n != len(data) {
		t.Fatalf("Read returned %d, want %d", n, len(data))
	}
	if !bytes.Equal(buf[:n], data) {
		t.Fatalf("Read got %q, want %q", buf[:n], data)
	}
	fs.Release("/wfile", fh)
}

func TestWriteAtOffset(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/offset", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/offset", []byte("AAA"), 0, fh)
	fs.Write("/offset", []byte("BBB"), 3, fh)
	fs.Flush("/offset", fh)

	buf := make([]byte, 64)
	n := fs.Read("/offset", buf, 0, fh)
	if n != 6 {
		t.Fatalf("Read returned %d, want 6", n)
	}
	if !bytes.Equal(buf[:6], []byte("AAABBB")) {
		t.Fatalf("got %q, want %q", buf[:6], "AAABBB")
	}
	fs.Release("/offset", fh)
}

func TestReadBeyondEOF(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/eof", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/eof", []byte("short"), 0, fh)
	fs.Flush("/eof", fh)
	fs.Release("/eof", fh)

	// Reopen for read
	errn, fh = fs.Open("/eof", 0)
	if errn != 0 {
		t.Fatalf("Open: %d", errn)
	}
	buf := make([]byte, 64)
	n := fs.Read("/eof", buf, 1000, fh)
	if n != 0 {
		t.Fatalf("Read beyond EOF returned %d, want 0", n)
	}
	fs.Release("/eof", fh)
}

func TestTruncateToZero(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/trunc0", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/trunc0", []byte("some data"), 0, fh)
	fs.Flush("/trunc0", fh)
	fs.Release("/trunc0", fh)

	errn = fs.Truncate("/trunc0", 0, math.MaxUint64)
	if errn != 0 {
		t.Fatalf("Truncate: %d", errn)
	}
	var stat fuse.Stat_t
	fs.Getattr("/trunc0", &stat, math.MaxUint64)
	if stat.Size != 0 {
		t.Fatalf("size after truncate = %d, want 0", stat.Size)
	}
}

func TestTruncateExtend(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/truncext", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/truncext", []byte("hi"), 0, fh)
	fs.Flush("/truncext", fh)
	fs.Release("/truncext", fh)

	errn = fs.Truncate("/truncext", 100, math.MaxUint64)
	if errn != 0 {
		t.Fatalf("Truncate extend: %d", errn)
	}
	var stat fuse.Stat_t
	fs.Getattr("/truncext", &stat, math.MaxUint64)
	if stat.Size != 100 {
		t.Fatalf("size after extend = %d, want 100", stat.Size)
	}
}

func TestUnlink(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/unlink", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Release("/unlink", fh)
	errn = fs.Unlink("/unlink")
	if errn != 0 {
		t.Fatalf("Unlink: %d", errn)
	}
	var stat fuse.Stat_t
	errn = fs.Getattr("/unlink", &stat, math.MaxUint64)
	if errn != -fuse.ENOENT {
		t.Fatalf("expected ENOENT, got %d", errn)
	}
}

func TestRenameFile(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/oldname", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Release("/oldname", fh)

	errn = fs.Rename("/oldname", "/newname")
	if errn != 0 {
		t.Fatalf("Rename: %d", errn)
	}
	var stat fuse.Stat_t
	errn = fs.Getattr("/oldname", &stat, math.MaxUint64)
	if errn != -fuse.ENOENT {
		t.Fatalf("old name should be gone, got %d", errn)
	}
	errn = fs.Getattr("/newname", &stat, math.MaxUint64)
	if errn != 0 {
		t.Fatalf("new name not found: %d", errn)
	}
}

func TestReaddir(t *testing.T) {
	fs := setupBumFS(t)
	fs.Mkdir("/rd", 0o755)
	fs.Create("/rd/a", 0, 0o644)
	fs.Create("/rd/b", 0, 0o644)

	errn, fh := fs.Opendir("/rd")
	if errn != 0 {
		t.Fatalf("Opendir: %d", errn)
	}

	var names []string
	fill := func(name string, stat *fuse.Stat_t, ofst int64) bool {
		names = append(names, name)
		return true
	}
	errn = fs.Readdir("/rd", fill, 0, fh)
	if errn != 0 {
		t.Fatalf("Readdir: %d", errn)
	}
	fs.Releasedir("/rd", fh)

	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	for _, expected := range []string{".", "..", "a", "b"} {
		if !nameSet[expected] {
			t.Fatalf("missing %q in readdir, got %v", expected, names)
		}
	}
}

func TestChmod(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/chm", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Release("/chm", fh)

	errn = fs.Chmod("/chm", 0o600)
	if errn != 0 {
		t.Fatalf("Chmod: %d", errn)
	}
	var stat fuse.Stat_t
	fs.Getattr("/chm", &stat, math.MaxUint64)
	if stat.Mode&0o7777 != 0o600 {
		t.Fatalf("mode = %o, want 0600 perm bits", stat.Mode)
	}
}

func TestChown(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/cho", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Release("/cho", fh)

	errn = fs.Chown("/cho", 1000, 1000)
	if errn != 0 {
		t.Fatalf("Chown: %d", errn)
	}
	var stat fuse.Stat_t
	fs.Getattr("/cho", &stat, math.MaxUint64)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Fatalf("uid=%d gid=%d", stat.Uid, stat.Gid)
	}
}

func TestUtimens(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/ut", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Release("/ut", fh)

	ts := []fuse.Timespec{{Sec: 1000, Nsec: 0}, {Sec: 2000, Nsec: 0}}
	errn = fs.Utimens("/ut", ts)
	if errn != 0 {
		t.Fatalf("Utimens: %d", errn)
	}
	var stat fuse.Stat_t
	fs.Getattr("/ut", &stat, math.MaxUint64)
	if stat.Atim.Sec != 1000 {
		t.Fatalf("atime = %d, want 1000", stat.Atim.Sec)
	}
	if stat.Mtim.Sec != 2000 {
		t.Fatalf("mtime = %d, want 2000", stat.Mtim.Sec)
	}
}

func TestFlushPersists(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/flush", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/flush", []byte("persisted"), 0, fh)
	fs.Flush("/flush", fh)
	fs.Release("/flush", fh)

	// Reopen and read
	errn, fh2 := fs.Open("/flush", 0)
	if errn != 0 {
		t.Fatalf("Open: %d", errn)
	}
	buf := make([]byte, 64)
	n := fs.Read("/flush", buf, 0, fh2)
	if n != 9 {
		t.Fatalf("Read returned %d, want 9", n)
	}
	if !bytes.Equal(buf[:9], []byte("persisted")) {
		t.Fatalf("got %q", buf[:9])
	}
	fs.Release("/flush", fh2)
}

func TestFsyncPersists(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/fsync", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/fsync", []byte("synced"), 0, fh)
	fs.Fsync("/fsync", false, fh)
	fs.Release("/fsync", fh)

	errn, fh2 := fs.Open("/fsync", 0)
	if errn != 0 {
		t.Fatalf("Open: %d", errn)
	}
	buf := make([]byte, 64)
	n := fs.Read("/fsync", buf, 0, fh2)
	if !bytes.Equal(buf[:n], []byte("synced")) {
		t.Fatalf("got %q", buf[:n])
	}
	fs.Release("/fsync", fh2)
}

func TestReleaseFlushesDirty(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/autoflush", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	fs.Write("/autoflush", []byte("auto"), 0, fh)
	// Release without explicit flush
	fs.Release("/autoflush", fh)

	errn, fh2 := fs.Open("/autoflush", 0)
	if errn != 0 {
		t.Fatalf("Open: %d", errn)
	}
	buf := make([]byte, 64)
	n := fs.Read("/autoflush", buf, 0, fh2)
	if !bytes.Equal(buf[:n], []byte("auto")) {
		t.Fatalf("got %q, want %q", buf[:n], "auto")
	}
	fs.Release("/autoflush", fh2)
}

func TestStatfs(t *testing.T) {
	fs := setupBumFS(t)
	var stat fuse.Statfs_t
	errn := fs.Statfs("/", &stat)
	if errn != 0 {
		t.Fatalf("Statfs: %d", errn)
	}
	if stat.Blocks == 0 {
		t.Fatal("expected non-zero blocks")
	}
}

func TestWriteBufferGrowth(t *testing.T) {
	fs := setupBumFS(t)
	errn, fh := fs.Create("/grow", 0, 0o644)
	if errn != 0 {
		t.Fatalf("Create: %d", errn)
	}
	// Write at a high offset — should zero-fill
	fs.Write("/grow", []byte("X"), 1000, fh)
	fs.Flush("/grow", fh)

	buf := make([]byte, 1001)
	n := fs.Read("/grow", buf, 0, fh)
	if n != 1001 {
		t.Fatalf("Read returned %d, want 1001", n)
	}
	// First 1000 bytes should be zero
	for i := 0; i < 1000; i++ {
		if buf[i] != 0 {
			t.Fatalf("byte %d = %d, want 0", i, buf[i])
		}
	}
	if buf[1000] != 'X' {
		t.Fatalf("byte 1000 = %d, want 'X'", buf[1000])
	}
	fs.Release("/grow", fh)
}
