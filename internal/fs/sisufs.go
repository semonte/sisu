package fs

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
	"github.com/smonte/sisu/internal/provider"
)

// Debug controls whether filesystem operations are logged
var Debug bool

// Config holds configuration for the filesystem
type Config struct {
	Profile string
	Region  string
}

// SisuFS is the main filesystem implementation
type SisuFS struct {
	pathfs.FileSystem
	providers    map[string]provider.Provider
	config       Config
	pendingFiles map[string]*writeableSisuFile // tracks files being written
	virtualDirs  map[string]bool               // tracks directories created via mkdir
	mu           sync.RWMutex
}

// NewSisuFS creates a new SisuFS instance
func NewSisuFS(cfg Config) (*SisuFS, error) {
	fs := &SisuFS{
		FileSystem:   pathfs.NewDefaultFileSystem(),
		providers:    make(map[string]provider.Provider),
		config:       cfg,
		pendingFiles: make(map[string]*writeableSisuFile),
		virtualDirs:  make(map[string]bool),
	}

	// Initialize providers
	s3Provider, err := provider.NewS3Provider(cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}
	fs.providers["s3"] = s3Provider

	ssmProvider, err := provider.NewSSMProvider(cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}
	fs.providers["ssm"] = ssmProvider

	vpcProvider, err := provider.NewVPCProvider(cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}
	fs.providers["vpc"] = vpcProvider

	iamProvider, err := provider.NewIAMProvider(cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}
	fs.providers["iam"] = iamProvider

	return fs, nil
}

// Mount mounts the filesystem at the given path
func (f *SisuFS) Mount(mountpoint string) (*fuse.Server, error) {
	nfs := pathfs.NewPathNodeFs(f, nil)
	opts := &nodefs.Options{
		AttrTimeout:  time.Second,
		EntryTimeout: time.Second,
	}

	server, _, err := nodefs.MountRoot(mountpoint, nfs.Root(), opts)
	if err != nil {
		return nil, err
	}

	go server.Serve()

	return server, nil
}

// ignoredFiles are files that shells/tools probe for that we should reject quickly
var ignoredFiles = map[string]bool{
	".git":        true,
	"HEAD":        true,
	".hg":         true,
	".svn":        true,
	".gitignore":  true,
	".gitmodules": true,
	".DS_Store":   true,
	"Thumbs.db":   true,
}

// GetAttr returns file attributes
func (f *SisuFS) GetAttr(name string, ctx *fuse.Context) (*fuse.Attr, fuse.Status) {
	if Debug {
		log.Printf("[fs] GetAttr: name=%q", name)
	}

	// Root directory
	if name == "" {
		return &fuse.Attr{
			Mode: fuse.S_IFDIR | 0777,
		}, fuse.OK
	}

	parts := strings.SplitN(name, "/", 2)
	providerName := parts[0]

	// Provider root directory
	if len(parts) == 1 {
		if _, ok := f.providers[providerName]; ok {
			return &fuse.Attr{
				Mode: fuse.S_IFDIR | 0777,
			}, fuse.OK
		}
		return nil, fuse.ENOENT
	}

	// Delegate to provider
	prov, ok := f.providers[providerName]
	if !ok {
		return nil, fuse.ENOENT
	}

	subpath := parts[1]

	// Quick reject for shell probe files
	baseName := subpath
	if idx := strings.LastIndex(subpath, "/"); idx >= 0 {
		baseName = subpath[idx+1:]
	}
	if ignoredFiles[baseName] {
		return nil, fuse.ENOENT
	}

	// Check if this is a file being written (not yet in S3)
	f.mu.RLock()
	if pending, ok := f.pendingFiles[name]; ok {
		f.mu.RUnlock()
		if Debug {
			log.Printf("[fs] GetAttr: returning pending file attrs for %q", name)
		}
		return &fuse.Attr{
			Mode: fuse.S_IFREG | 0666,
			Size: uint64(pending.buf.Len()),
		}, fuse.OK
	}
	// Check if this is a virtual directory created via mkdir
	if f.virtualDirs[name] {
		f.mu.RUnlock()
		if Debug {
			log.Printf("[fs] GetAttr: returning virtual dir attrs for %q", name)
		}
		return &fuse.Attr{
			Mode: fuse.S_IFDIR | 0777,
		}, fuse.OK
	}
	f.mu.RUnlock()

	entry, err := prov.Stat(context.Background(), subpath)
	if err != nil {
		return nil, fuse.ENOENT
	}

	attr := &fuse.Attr{
		Size:  uint64(entry.Size),
		Mtime: uint64(entry.ModTime.Unix()),
	}

	if entry.IsDir {
		attr.Mode = fuse.S_IFDIR | 0777
	} else {
		attr.Mode = fuse.S_IFREG | 0666
	}

	return attr, fuse.OK
}

// Access checks file access permissions
func (f *SisuFS) Access(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	if Debug {
		log.Printf("[fs] Access: name=%q mode=%d", name, mode)
	}
	return fuse.OK
}

// Mkdir creates a directory (for SSM, this is a no-op since directories are virtual)
func (f *SisuFS) Mkdir(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	if Debug {
		log.Printf("[fs] Mkdir: name=%q mode=%d", name, mode)
	}

	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 {
		return fuse.EPERM
	}

	providerName := parts[0]
	_, ok := f.providers[providerName]
	if !ok {
		return fuse.ENOENT
	}

	// For SSM and S3, directories are virtual - track them locally
	f.mu.Lock()
	f.virtualDirs[name] = true
	f.mu.Unlock()

	return fuse.OK
}

// Unlink deletes a file
func (f *SisuFS) Unlink(name string, ctx *fuse.Context) fuse.Status {
	if Debug {
		log.Printf("[fs] Unlink: name=%q", name)
	}

	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 {
		return fuse.EPERM
	}

	providerName := parts[0]
	prov, ok := f.providers[providerName]
	if !ok {
		return fuse.ENOENT
	}

	subpath := parts[1]
	err := prov.Delete(context.Background(), subpath)
	if err != nil {
		if Debug {
			log.Printf("[fs] Unlink failed: %v", err)
		}
		return fuse.EIO
	}

	return fuse.OK
}

// OpenDir opens a directory for reading
func (f *SisuFS) OpenDir(name string, ctx *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	// Root directory - list providers
	if name == "" {
		entries := make([]fuse.DirEntry, 0, len(f.providers))
		for provName := range f.providers {
			entries = append(entries, fuse.DirEntry{
				Name: provName,
				Mode: fuse.S_IFDIR | 0755,
			})
		}
		return entries, fuse.OK
	}

	parts := strings.SplitN(name, "/", 2)
	providerName := parts[0]

	prov, ok := f.providers[providerName]
	if !ok {
		return nil, fuse.ENOENT
	}

	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	provEntries, err := prov.ReadDir(context.Background(), subpath)
	if err != nil {
		// Check if this is a virtual directory
		f.mu.RLock()
		isVirtual := f.virtualDirs[name]
		f.mu.RUnlock()
		if isVirtual {
			return []fuse.DirEntry{}, fuse.OK
		}
		return nil, fuse.EIO
	}

	entries := make([]fuse.DirEntry, len(provEntries))
	for i, e := range provEntries {
		mode := fuse.S_IFREG | 0644
		if e.IsDir {
			mode = fuse.S_IFDIR | 0755
		}
		entries[i] = fuse.DirEntry{
			Name: e.Name,
			Mode: uint32(mode),
		}
	}

	return entries, fuse.OK
}

// Open opens a file for reading
func (f *SisuFS) Open(name string, flags uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	if Debug {
		log.Printf("[fs] Open: name=%q flags=%d", name, flags)
	}

	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 {
		return nil, fuse.ENOENT
	}

	providerName := parts[0]
	prov, ok := f.providers[providerName]
	if !ok {
		return nil, fuse.ENOENT
	}

	subpath := parts[1]
	data, err := prov.Read(context.Background(), subpath)
	if err != nil {
		if Debug {
			log.Printf("[fs] Open: Read failed for %q: %v", name, err)
		}
		return nil, fuse.EIO
	}

	if Debug {
		log.Printf("[fs] Open: Read %d bytes for %q", len(data), name)
	}

	return &sisuFile{
		File: nodefs.NewDefaultFile(),
		data: data,
	}, fuse.OK
}

// Create creates a new file for writing
func (f *SisuFS) Create(name string, flags uint32, mode uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	if Debug {
		log.Printf("[fs] Create called: name=%q flags=%d mode=%d", name, flags, mode)
	}

	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 {
		if Debug {
			log.Printf("[fs] Create failed: path too short, parts=%v", parts)
		}
		return nil, fuse.EPERM
	}

	providerName := parts[0]
	prov, ok := f.providers[providerName]
	if !ok {
		if Debug {
			log.Printf("[fs] Create failed: unknown provider %q", providerName)
		}
		return nil, fuse.ENOENT
	}

	subpath := parts[1]
	if Debug {
		log.Printf("[fs] Create: provider=%q subpath=%q", providerName, subpath)
	}

	wf := &writeableSisuFile{
		File: nodefs.NewDefaultFile(),
		prov: prov,
		path: subpath,
		fs:   f,
		name: name,
	}

	// Register as pending so GetAttr works
	f.mu.Lock()
	f.pendingFiles[name] = wf
	f.mu.Unlock()

	return wf, fuse.OK
}

// sisuFile is a simple in-memory file
type sisuFile struct {
	nodefs.File
	data []byte
}

func (f *sisuFile) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	end := off + int64(len(buf))
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), fuse.OK
	}
	return fuse.ReadResultData(f.data[off:end]), fuse.OK
}

func (f *sisuFile) GetAttr(out *fuse.Attr) fuse.Status {
	out.Mode = fuse.S_IFREG | 0644
	out.Size = uint64(len(f.data))
	return fuse.OK
}

func (f *sisuFile) Release()                        {}
func (f *sisuFile) Flush() fuse.Status              { return fuse.OK }
func (f *sisuFile) Fsync(flags int) fuse.Status     { return fuse.OK }
func (f *sisuFile) Truncate(size uint64) fuse.Status { return fuse.Status(syscall.EROFS) }
func (f *sisuFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	return 0, fuse.Status(syscall.EROFS)
}

// writeableSisuFile is a file that buffers writes and flushes to provider
type writeableSisuFile struct {
	nodefs.File
	prov provider.Provider
	path string
	buf  bytes.Buffer
	fs   *SisuFS // reference to parent filesystem
	name string // full path name for pending files tracking
}

func (f *writeableSisuFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	if Debug {
		log.Printf("[fs] writeableSisuFile.Write: path=%q off=%d len=%d", f.path, off, len(data))
	}
	// For simplicity, we only support sequential writes from offset 0
	// This covers the common case of cp/cat > file
	if off == 0 {
		f.buf.Reset()
	}
	n, err := f.buf.Write(data)
	if err != nil {
		return 0, fuse.EIO
	}
	return uint32(n), fuse.OK
}

func (f *writeableSisuFile) Flush() fuse.Status {
	if Debug {
		log.Printf("[fs] writeableSisuFile.Flush: path=%q bufLen=%d", f.path, f.buf.Len())
	}
	if f.buf.Len() == 0 {
		return fuse.OK
	}
	err := f.prov.Write(context.Background(), f.path, f.buf.Bytes())
	if err != nil {
		if Debug {
			log.Printf("[fs] writeableSisuFile.Flush failed: %v", err)
		}
		return fuse.EIO
	}
	return fuse.OK
}

func (f *writeableSisuFile) Release() {
	if Debug {
		log.Printf("[fs] writeableSisuFile.Release: path=%q", f.path)
	}
	// Remove from pending files
	if f.fs != nil {
		f.fs.mu.Lock()
		delete(f.fs.pendingFiles, f.name)
		f.fs.mu.Unlock()
	}
	f.buf.Reset()
}

func (f *writeableSisuFile) GetAttr(out *fuse.Attr) fuse.Status {
	out.Mode = fuse.S_IFREG | 0644
	out.Size = uint64(f.buf.Len())
	return fuse.OK
}

func (f *writeableSisuFile) Truncate(size uint64) fuse.Status {
	if size == 0 {
		f.buf.Reset()
	}
	return fuse.OK
}
