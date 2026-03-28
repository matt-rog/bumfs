package fs

import (
	"context"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/matt-rog/bumfs/internal/chunk"
	"github.com/matt-rog/bumfs/internal/meta"
	"github.com/matt-rog/bumfs/internal/store"
	"github.com/winfsp/cgofuse/fuse"
)

// handle represents an open file handle with a write buffer.
type handle struct {
	ino   uint64
	dirty bool
	buf   []byte
}

// BumFS implements cgofuse FileSystemInterface.
type BumFS struct {
	fuse.FileSystemBase
	mu      sync.RWMutex
	meta    *meta.Store
	chunks  *chunk.Manager
	backend store.StorageConnector

	handleMu  sync.Mutex
	handles   map[uint64]*handle
	nextFH    uint64
}

// New creates a new BumFS.
func New(metaStore *meta.Store, chunkMgr *chunk.Manager, backend store.StorageConnector) *BumFS {
	return &BumFS{
		meta:    metaStore,
		chunks:  chunkMgr,
		backend: backend,
		handles: make(map[uint64]*handle),
		nextFH:  1,
	}
}

func errno(err error) int {
	if err == nil {
		return 0
	}
	if os.IsNotExist(err) {
		return -fuse.ENOENT
	}
	if os.IsExist(err) {
		return -fuse.EEXIST
	}
	if os.IsPermission(err) {
		return -fuse.EACCES
	}
	// Check for "inode not found" from our meta layer
	if err.Error() == "meta: inode not found" {
		return -fuse.ENOENT
	}
	log.Printf("bumfs error: %v", err)
	return -fuse.EIO
}

func fillStat(stat *fuse.Stat_t, st *meta.InodeStat) {
	stat.Ino = st.Ino
	stat.Mode = st.Mode
	stat.Nlink = uint32(st.Nlink)
	stat.Uid = st.Uid
	stat.Gid = st.Gid
	stat.Size = st.Size
	stat.Atim = fuse.Timespec{Sec: st.AtimeSec, Nsec: st.AtimeNsec}
	stat.Mtim = fuse.Timespec{Sec: st.MtimeSec, Nsec: st.MtimeNsec}
	stat.Ctim = fuse.Timespec{Sec: st.CtimeSec, Nsec: st.CtimeNsec}
	stat.Blksize = 4096
	stat.Blocks = (st.Size + 511) / 512
}

func (fs *BumFS) allocHandle(ino uint64) uint64 {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	fh := fs.nextFH
	fs.nextFH++
	fs.handles[fh] = &handle{ino: ino}
	return fh
}

func (fs *BumFS) getHandle(fh uint64) *handle {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	return fs.handles[fh]
}

func (fs *BumFS) releaseHandle(fh uint64) *handle {
	fs.handleMu.Lock()
	defer fs.handleMu.Unlock()
	h := fs.handles[fh]
	delete(fs.handles, fh)
	return h
}

// resolvePath resolves a FUSE path to an InodeStat.
func (fs *BumFS) resolvePath(path string) (*meta.InodeStat, int) {
	st, err := fs.meta.LookupPath(path)
	if err != nil {
		return nil, errno(err)
	}
	return st, 0
}

// parentAndName splits a path into parent directory path and base name.
func parentAndName(path string) (string, string) {
	if path == "/" {
		return "/", ""
	}
	// Find last slash
	i := len(path) - 1
	for i > 0 && path[i] != '/' {
		i--
	}
	parent := path[:i]
	if parent == "" {
		parent = "/"
	}
	name := path[i+1:]
	return parent, name
}

// Init is called when the filesystem is mounted.
func (fs *BumFS) Init() {
	log.Println("bumfs: mounted")
}

// Destroy is called when the filesystem is unmounted.
func (fs *BumFS) Destroy() {
	log.Println("bumfs: unmounting")
}

// Statfs reports filesystem statistics.
func (fs *BumFS) Statfs(path string, stat *fuse.Statfs_t) int {
	total, used, free := fs.backend.Capacity()
	blockSize := uint64(4096)
	stat.Bsize = uint64(blockSize)
	stat.Frsize = uint64(blockSize)
	stat.Blocks = total / blockSize
	stat.Bfree = free / blockSize
	stat.Bavail = free / blockSize
	_ = used
	stat.Namemax = 255
	return 0
}

// Getattr returns attributes for a path.
func (fs *BumFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// Check if we have a dirty handle and use its buffer size
	if fh != math.MaxUint64 {
		h := fs.getHandle(fh)
		if h != nil && h.dirty {
			st, errn := fs.resolvePath(path)
			if errn != 0 {
				return errn
			}
			fillStat(stat, st)
			stat.Size = int64(len(h.buf))
			return 0
		}
	}

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn
	}
	fillStat(stat, st)
	return 0
}

// Mkdir creates a directory.
func (fs *BumFS) Mkdir(path string, mode uint32) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, name := parentAndName(path)
	pst, errn := fs.resolvePath(parent)
	if errn != 0 {
		return errn
	}

	_, err := fs.meta.CreateNode(pst.Ino, name, 0o040000|mode, uint32(os.Getuid()), uint32(os.Getgid()))
	return errno(err)
}

// Rmdir removes a directory.
func (fs *BumFS) Rmdir(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, name := parentAndName(path)
	pst, errn := fs.resolvePath(parent)
	if errn != 0 {
		return errn
	}

	// Check directory is empty
	child, err := fs.meta.LookupChild(pst.Ino, name)
	if err != nil {
		return errno(err)
	}
	entries, err := fs.meta.ListDir(child.Ino)
	if err != nil {
		return errno(err)
	}
	if len(entries) > 0 {
		return -fuse.ENOTEMPTY
	}

	return errno(fs.meta.RemoveNode(pst.Ino, name))
}

// Mknod creates a file node.
func (fs *BumFS) Mknod(path string, mode uint32, dev uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, name := parentAndName(path)
	pst, errn := fs.resolvePath(parent)
	if errn != 0 {
		return errn
	}

	_, err := fs.meta.CreateNode(pst.Ino, name, mode, uint32(os.Getuid()), uint32(os.Getgid()))
	return errno(err)
}

// Create creates and opens a file.
func (fs *BumFS) Create(path string, flags int, mode uint32) (int, uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, name := parentAndName(path)
	pst, errn := fs.resolvePath(parent)
	if errn != 0 {
		return errn, math.MaxUint64
	}

	ino, err := fs.meta.CreateNode(pst.Ino, name, 0o100000|mode, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		return errno(err), math.MaxUint64
	}

	fh := fs.allocHandle(ino)
	return 0, fh
}

// Open opens a file.
func (fs *BumFS) Open(path string, flags int) (int, uint64) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn, math.MaxUint64
	}

	fh := fs.allocHandle(st.Ino)

	// If opening for write with truncate, clear the buffer
	if flags&os.O_TRUNC != 0 {
		h := fs.getHandle(fh)
		h.buf = nil
		h.dirty = true
	}

	return 0, fh
}

// Release closes a file handle.
func (fs *BumFS) Release(path string, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	h := fs.releaseHandle(fh)
	if h != nil && h.dirty {
		if errn := fs.flushHandle(h); errn != 0 {
			return errn
		}
	}
	return 0
}

// Read reads data from a file.
func (fs *BumFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	h := fs.getHandle(fh)
	if h == nil {
		return -fuse.EBADF
	}

	// If handle has buffered data, read from buffer
	if h.dirty {
		if ofst >= int64(len(h.buf)) {
			return 0
		}
		n := copy(buff, h.buf[ofst:])
		return n
	}

	// Read from chunk store
	st, err := fs.meta.GetInode(h.ino)
	if err != nil {
		return errno(err)
	}
	if st.Size == 0 {
		return 0
	}

	fileChunks, err := fs.meta.GetFileChunks(h.ino)
	if err != nil {
		return errno(err)
	}

	refs := make([]chunk.Ref, len(fileChunks))
	for i, fc := range fileChunks {
		refs[i] = chunk.Ref{ChunkID: fc.ChunkID, Index: fc.ChunkIdx}
	}

	data, err := fs.chunks.ReadFile(context.Background(), refs)
	if err != nil {
		log.Printf("bumfs: read error: %v", err)
		return -fuse.EIO
	}

	if ofst >= int64(len(data)) {
		return 0
	}
	n := copy(buff, data[ofst:])
	return n
}

// Write writes data to a file (buffered).
func (fs *BumFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	h := fs.getHandle(fh)
	if h == nil {
		return -fuse.EBADF
	}

	// If first write on a non-dirty handle, load existing data
	if !h.dirty {
		existing, err := fs.loadFileData(h.ino)
		if err != nil {
			log.Printf("bumfs: write load error: %v", err)
			return -fuse.EIO
		}
		h.buf = existing
		h.dirty = true
	}

	// Extend buffer if needed
	end := int(ofst) + len(buff)
	if end > len(h.buf) {
		newBuf := make([]byte, end)
		copy(newBuf, h.buf)
		h.buf = newBuf
	}

	copy(h.buf[ofst:], buff)
	return len(buff)
}

// Truncate resizes a file.
func (fs *BumFS) Truncate(path string, size int64, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	var h *handle
	if fh != math.MaxUint64 {
		h = fs.getHandle(fh)
	}

	if h != nil {
		// Handle open with buffer
		if !h.dirty {
			existing, err := fs.loadFileData(h.ino)
			if err != nil {
				return -fuse.EIO
			}
			h.buf = existing
			h.dirty = true
		}
		if int64(len(h.buf)) < size {
			newBuf := make([]byte, size)
			copy(newBuf, h.buf)
			h.buf = newBuf
		} else {
			h.buf = h.buf[:size]
		}
		return 0
	}

	// No open handle — resolve and truncate directly
	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn
	}

	if size == 0 {
		// Delete all chunks
		fileChunks, err := fs.meta.GetFileChunks(st.Ino)
		if err != nil {
			return errno(err)
		}
		refs := make([]chunk.Ref, len(fileChunks))
		for i, fc := range fileChunks {
			refs[i] = chunk.Ref{ChunkID: fc.ChunkID, Index: fc.ChunkIdx}
		}
		if len(refs) > 0 {
			fs.chunks.DeleteChunks(context.Background(), refs)
		}
		fs.meta.SetFileChunks(st.Ino, nil)
		fs.meta.UpdateSize(st.Ino, 0)
		return 0
	}

	// Non-zero truncate: read, resize, rewrite
	data, err := fs.loadFileData(st.Ino)
	if err != nil {
		return -fuse.EIO
	}
	if int64(len(data)) < size {
		newData := make([]byte, size)
		copy(newData, data)
		data = newData
	} else {
		data = data[:size]
	}

	if errn := fs.writeFileData(st.Ino, data); errn != 0 {
		return errn
	}
	return 0
}

// Unlink removes a file.
func (fs *BumFS) Unlink(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, name := parentAndName(path)
	pst, errn := fs.resolvePath(parent)
	if errn != 0 {
		return errn
	}

	child, err := fs.meta.LookupChild(pst.Ino, name)
	if err != nil {
		return errno(err)
	}

	// Delete chunks from backend
	fileChunks, err := fs.meta.GetFileChunks(child.Ino)
	if err != nil {
		return errno(err)
	}
	refs := make([]chunk.Ref, len(fileChunks))
	for i, fc := range fileChunks {
		refs[i] = chunk.Ref{ChunkID: fc.ChunkID, Index: fc.ChunkIdx}
	}

	if err := fs.meta.RemoveNode(pst.Ino, name); err != nil {
		return errno(err)
	}

	// Clean up unreferenced chunks from backend
	unreferenced, err := fs.meta.GetUnreferencedChunks()
	if err == nil {
		for _, id := range unreferenced {
			fs.backend.Delete(context.Background(), id)
			fs.meta.DeleteChunkRecord(id)
		}
	}

	return 0
}

// Rename moves/renames a file or directory.
func (fs *BumFS) Rename(oldpath string, newpath string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	oldParent, oldName := parentAndName(oldpath)
	newParent, newName := parentAndName(newpath)

	oldPst, errn := fs.resolvePath(oldParent)
	if errn != 0 {
		return errn
	}
	newPst, errn := fs.resolvePath(newParent)
	if errn != 0 {
		return errn
	}

	return errno(fs.meta.Rename(oldPst.Ino, oldName, newPst.Ino, newName))
}

// Opendir opens a directory.
func (fs *BumFS) Opendir(path string) (int, uint64) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn, math.MaxUint64
	}
	fh := fs.allocHandle(st.Ino)
	return 0, fh
}

// Releasedir closes a directory handle.
func (fs *BumFS) Releasedir(path string, fh uint64) int {
	fs.releaseHandle(fh)
	return 0
}

// Readdir lists directory entries.
func (fs *BumFS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64, fh uint64) int {

	fs.mu.RLock()
	defer fs.mu.RUnlock()

	h := fs.getHandle(fh)
	if h == nil {
		return -fuse.EBADF
	}

	// "." and ".."
	fill(".", nil, 0)
	fill("..", nil, 0)

	entries, err := fs.meta.ListDir(h.ino)
	if err != nil {
		return errno(err)
	}

	for _, entry := range entries {
		var st fuse.Stat_t
		fillStat(&st, &entry)
		if !fill(entry.Name, &st, 0) {
			break
		}
	}
	return 0
}

// Chmod changes file mode.
func (fs *BumFS) Chmod(path string, mode uint32) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn
	}
	// Preserve file type bits, only change permission bits
	newMode := (st.Mode & 0o170000) | (mode & 0o7777)
	return errno(fs.meta.Chmod(st.Ino, newMode))
}

// Chown changes file ownership.
func (fs *BumFS) Chown(path string, uid uint32, gid uint32) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn
	}
	return errno(fs.meta.Chown(st.Ino, uid, gid))
}

// Utimens changes file access and modification times.
func (fs *BumFS) Utimens(path string, tmsp []fuse.Timespec) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	st, errn := fs.resolvePath(path)
	if errn != 0 {
		return errn
	}

	atimeSec := st.AtimeSec
	atimeNsec := st.AtimeNsec
	mtimeSec := st.MtimeSec
	mtimeNsec := st.MtimeNsec

	if len(tmsp) >= 1 {
		atimeSec = tmsp[0].Sec
		atimeNsec = tmsp[0].Nsec
	}
	if len(tmsp) >= 2 {
		mtimeSec = tmsp[1].Sec
		mtimeNsec = tmsp[1].Nsec
	}

	return errno(fs.meta.UpdateTimes(st.Ino, atimeSec, atimeNsec, mtimeSec, mtimeNsec))
}

// Flush is called when a file handle is flushed (e.g. on close).
func (fs *BumFS) Flush(path string, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	h := fs.getHandle(fh)
	if h != nil && h.dirty {
		return fs.flushHandle(h)
	}
	return 0
}

// Fsync persists file data.
func (fs *BumFS) Fsync(path string, datasync bool, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	h := fs.getHandle(fh)
	if h != nil && h.dirty {
		return fs.flushHandle(h)
	}
	return 0
}

// flushHandle writes buffered data to the chunk store. Caller must hold fs.mu.
func (fs *BumFS) flushHandle(h *handle) int {
	return fs.writeFileData(h.ino, h.buf)
}

// writeFileData writes data for an inode to the chunk store and updates metadata.
func (fs *BumFS) writeFileData(ino uint64, data []byte) int {
	// Delete old chunks from backend
	oldChunks, err := fs.meta.GetFileChunks(ino)
	if err == nil && len(oldChunks) > 0 {
		for _, oc := range oldChunks {
			fs.backend.Delete(context.Background(), oc.ChunkID)
		}
	}

	if len(data) == 0 {
		fs.meta.SetFileChunks(ino, nil)
		fs.meta.UpdateSize(ino, 0)
		return 0
	}

	refs, err := fs.chunks.WriteFile(context.Background(), data)
	if err != nil {
		log.Printf("bumfs: flush write error: %v", err)
		return -fuse.EIO
	}

	chunkInfos := make([]struct {
		ChunkID string
		Size    int64
		Backend string
	}, len(refs))
	for i, r := range refs {
		chunkInfos[i].ChunkID = r.ChunkID
		chunkInfos[i].Size = int64(r.Size)
		chunkInfos[i].Backend = fs.backend.Name()
	}

	if err := fs.meta.SetFileChunks(ino, chunkInfos); err != nil {
		log.Printf("bumfs: flush set chunks error: %v", err)
		return -fuse.EIO
	}

	if err := fs.meta.UpdateSize(ino, int64(len(data))); err != nil {
		log.Printf("bumfs: flush update size error: %v", err)
		return -fuse.EIO
	}

	// Clean up unreferenced chunks
	unreferenced, err := fs.meta.GetUnreferencedChunks()
	if err == nil {
		for _, id := range unreferenced {
			fs.backend.Delete(context.Background(), id)
			fs.meta.DeleteChunkRecord(id)
		}
	}

	return 0
}

// loadFileData reads the full contents of a file from the chunk store.
func (fs *BumFS) loadFileData(ino uint64) ([]byte, error) {
	st, err := fs.meta.GetInode(ino)
	if err != nil {
		return nil, err
	}
	if st.Size == 0 {
		return nil, nil
	}

	fileChunks, err := fs.meta.GetFileChunks(ino)
	if err != nil {
		return nil, err
	}
	if len(fileChunks) == 0 {
		return nil, nil
	}

	refs := make([]chunk.Ref, len(fileChunks))
	for i, fc := range fileChunks {
		refs[i] = chunk.Ref{ChunkID: fc.ChunkID, Index: fc.ChunkIdx}
	}

	data, err := fs.chunks.ReadFile(context.Background(), refs)
	if err != nil {
		return nil, err
	}

	// Respect metadata size (in case of truncation)
	if int64(len(data)) > st.Size {
		data = data[:st.Size]
	}
	return data, nil
}

// nowTimespec returns the current time as a fuse.Timespec.
func nowTimespec() fuse.Timespec {
	t := time.Now()
	return fuse.Timespec{Sec: t.Unix(), Nsec: int64(t.Nanosecond())}
}
