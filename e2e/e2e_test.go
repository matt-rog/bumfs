//go:build fuse3

package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/matt-rog/bumfs/internal/chunk"
	"github.com/matt-rog/bumfs/internal/crypto"
	bumfs "github.com/matt-rog/bumfs/internal/fs"
	"github.com/matt-rog/bumfs/internal/meta"
	"github.com/matt-rog/bumfs/internal/store/local"
	"github.com/winfsp/cgofuse/fuse"
)

func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); os.IsNotExist(err) {
		t.Skip("skipping: /dev/fuse not available")
	}
}

// mountTestFS mounts a real FUSE filesystem and returns the mount point and chunks directory.
// The filesystem is unmounted on test cleanup.
func mountTestFS(t *testing.T) (mountpoint string, chunksDir string) {
	t.Helper()
	skipIfNoFUSE(t)

	dir := t.TempDir()
	mountpoint = filepath.Join(dir, "mnt")
	chunksDir = filepath.Join(dir, "chunks")
	dbPath := filepath.Join(dir, "meta.db")

	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		t.Fatalf("mkdir mount: %v", err)
	}

	metaStore, err := meta.Open(dbPath)
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	enc, err := crypto.NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	backend, err := local.New(chunksDir)
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	chunkMgr := chunk.NewManager(0, enc, backend)
	fsys := bumfs.New(metaStore, chunkMgr, backend)
	host := fuse.NewFileSystemHost(fsys)

	mounted := make(chan struct{})
	mountDone := make(chan struct{})
	go func() {
		defer close(mountDone)
		host.Mount(mountpoint, nil)
	}()

	// Poll until mount is ready by verifying it's a FUSE filesystem
	go func() {
		for i := 0; i < 200; i++ {
			var st syscall.Statfs_t
			if err := syscall.Statfs(mountpoint, &st); err == nil {
				// FUSE filesystems have magic number 0x65735546
				if st.Type == 0x65735546 {
					close(mounted)
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case <-mounted:
	case <-time.After(10 * time.Second):
		t.Fatal("FUSE mount timed out")
	}

	t.Cleanup(func() {
		// Unmount via cgofuse first, then fall back to fusermount3
		host.Unmount()
		select {
		case <-mountDone:
		case <-time.After(2 * time.Second):
			// Force unmount via fusermount3 if cgofuse didn't work
			exec.Command("fusermount3", "-uz", mountpoint).Run()
			select {
			case <-mountDone:
			case <-time.After(3 * time.Second):
			}
		}
		metaStore.Close()
	})

	return mountpoint, chunksDir
}

func TestMountUnmount(t *testing.T) {
	mp, _ := mountTestFS(t)
	info, err := os.Stat(mp)
	if err != nil {
		t.Fatalf("Stat mount point: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("mount point should be a directory")
	}
}

func TestCreateAndReadFile(t *testing.T) {
	mp, _ := mountTestFS(t)
	data := []byte("hello bumfs e2e")
	path := filepath.Join(mp, "test.txt")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestCreateEmptyFile(t *testing.T) {
	mp, _ := mountTestFS(t)
	path := filepath.Join(mp, "empty.txt")
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d, want 0", info.Size())
	}
}

func TestLargeFileRoundTrip(t *testing.T) {
	mp, _ := mountTestFS(t)
	data := make([]byte, 5*1024*1024)
	rand.Read(data)
	path := filepath.Join(mp, "large.bin")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("large file round-trip mismatch")
	}
}

func TestOverwriteFile(t *testing.T) {
	mp, _ := mountTestFS(t)
	path := filepath.Join(mp, "overwrite.txt")
	os.WriteFile(path, []byte("AAA"), 0644)
	os.WriteFile(path, []byte("BBB"), 0644)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("BBB")) {
		t.Fatalf("got %q, want %q", got, "BBB")
	}
}

func TestMkdirAndReadDir(t *testing.T) {
	mp, _ := mountTestFS(t)
	dirPath := filepath.Join(mp, "subdir")
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entries, err := os.ReadDir(mp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "subdir" && e.IsDir() {
			found = true
		}
	}
	if !found {
		t.Fatal("subdir not found in listing")
	}
}

func TestNestedDirectories(t *testing.T) {
	mp, _ := mountTestFS(t)
	deep := filepath.Join(mp, "a", "b", "c")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(deep)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestRemoveFile(t *testing.T) {
	mp, _ := mountTestFS(t)
	path := filepath.Join(mp, "removeme.txt")
	os.WriteFile(path, []byte("gone"), 0644)
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should not exist after remove")
	}
}

func TestRemoveDirectory(t *testing.T) {
	mp, _ := mountTestFS(t)
	dirPath := filepath.Join(mp, "rmdir")
	os.Mkdir(dirPath, 0755)
	if err := os.Remove(dirPath); err != nil {
		t.Fatalf("Remove dir: %v", err)
	}
	if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
		t.Fatal("directory should not exist")
	}
}

func TestRemoveNonEmptyDirFails(t *testing.T) {
	mp, _ := mountTestFS(t)
	dirPath := filepath.Join(mp, "notempty")
	os.Mkdir(dirPath, 0755)
	os.WriteFile(filepath.Join(dirPath, "child"), []byte("x"), 0644)
	err := os.Remove(dirPath)
	if err == nil {
		t.Fatal("expected error removing non-empty dir")
	}
}

func TestRenameFile(t *testing.T) {
	mp, _ := mountTestFS(t)
	oldPath := filepath.Join(mp, "old.txt")
	newPath := filepath.Join(mp, "new.txt")
	os.WriteFile(oldPath, []byte("data"), 0644)
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old path should be gone")
	}
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("data")) {
		t.Fatalf("got %q", got)
	}
}

func TestRenameAcrossDirectories(t *testing.T) {
	mp, _ := mountTestFS(t)
	os.Mkdir(filepath.Join(mp, "src"), 0755)
	os.Mkdir(filepath.Join(mp, "dst"), 0755)
	srcFile := filepath.Join(mp, "src", "file.txt")
	dstFile := filepath.Join(mp, "dst", "file.txt")
	os.WriteFile(srcFile, []byte("moved"), 0644)
	if err := os.Rename(srcFile, dstFile); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("moved")) {
		t.Fatalf("got %q", got)
	}
}

func TestFilePermissions(t *testing.T) {
	mp, _ := mountTestFS(t)
	path := filepath.Join(mp, "perms.txt")
	os.WriteFile(path, []byte("x"), 0644)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("perm = %o, want 0600", info.Mode().Perm())
	}
}

func TestTruncate(t *testing.T) {
	mp, _ := mountTestFS(t)
	path := filepath.Join(mp, "trunc.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	// Truncate to zero
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate to 0: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Size() != 0 {
		t.Fatalf("size = %d after truncate to 0", info.Size())
	}

	// Extend
	os.WriteFile(path, []byte("hi"), 0644)
	if err := os.Truncate(path, 100); err != nil {
		t.Fatalf("Truncate extend: %v", err)
	}
	info, _ = os.Stat(path)
	if info.Size() != 100 {
		t.Fatalf("size = %d after extend to 100", info.Size())
	}
}

func TestConcurrentWrites(t *testing.T) {
	mp, _ := mountTestFS(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("concurrent_%d.txt", idx)
			path := filepath.Join(mp, name)
			data := []byte(fmt.Sprintf("data from goroutine %d", idx))
			if err := os.WriteFile(path, data, 0644); err != nil {
				errs <- fmt.Errorf("write %s: %w", name, err)
				return
			}
			got, err := os.ReadFile(path)
			if err != nil {
				errs <- fmt.Errorf("read %s: %w", name, err)
				return
			}
			if !bytes.Equal(got, data) {
				errs <- fmt.Errorf("%s: got %q, want %q", name, got, data)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestEncryptionAtRest(t *testing.T) {
	mp, chunksDir := mountTestFS(t)
	plaintext := []byte("super secret data that must be encrypted")
	path := filepath.Join(mp, "secret.txt")

	// Write file and verify it exists
	if err := os.WriteFile(path, plaintext, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify the file is readable (ensures flush completed)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("read-back mismatch")
	}

	// Read all chunk files from the chunks directory (read directly, bypassing FUSE)
	chunkFiles, err := os.ReadDir(chunksDir)
	if err != nil {
		t.Fatalf("ReadDir chunks: %v", err)
	}
	if len(chunkFiles) == 0 {
		t.Fatal("expected chunk files on disk")
	}

	for _, cf := range chunkFiles {
		raw, err := os.ReadFile(filepath.Join(chunksDir, cf.Name()))
		if err != nil {
			t.Fatalf("ReadFile chunk: %v", err)
		}
		if strings.Contains(string(raw), string(plaintext)) {
			t.Fatal("plaintext found in raw chunk — encryption not working")
		}
	}
}

func TestContentAddressingDedup(t *testing.T) {
	mp, chunksDir := mountTestFS(t)
	data := bytes.Repeat([]byte("dedup test content"), 100)

	// Write same data to two files
	os.WriteFile(filepath.Join(mp, "file1.txt"), data, 0644)

	chunks1, _ := os.ReadDir(chunksDir)
	count1 := len(chunks1)

	os.WriteFile(filepath.Join(mp, "file2.txt"), data, 0644)

	chunks2, _ := os.ReadDir(chunksDir)
	count2 := len(chunks2)

	// Content-addressed storage means the second file should reuse chunks
	// so the chunk count should remain the same
	if count2 != count1 {
		t.Fatalf("chunk count changed from %d to %d; expected dedup", count1, count2)
	}

	// Verify both files read correctly
	for _, name := range []string{"file1.txt", "file2.txt"} {
		got, err := os.ReadFile(filepath.Join(mp, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%s content mismatch", name)
		}
	}
}
