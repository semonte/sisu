package fs

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
	"github.com/semonte/sisu/internal/provider"
	"gopkg.in/ini.v1"
)

// Debug controls whether filesystem operations are logged
var Debug bool

// Config holds configuration for the filesystem
type Config struct {
	Profile  string
	Region   string
	Regions  []string // regions to show
}

// Global services that don't need a region
var globalServices = map[string]bool{
	"iam": true,
	"s3":  true,
}

// Regional services
var regionalServices = []string{"ssm", "vpc", "lambda", "ec2"}

// Writable services (support write/delete)
var writableServices = map[string]bool{
	"s3":  true,
	"ssm": true,
}

// Default regions to show
var defaultRegions = []string{"us-east-1", "us-west-2", "eu-west-1", "eu-central-1", "ap-northeast-1"}

// SisuFS is the main filesystem implementation
type SisuFS struct {
	pathfs.FileSystem
	config       Config
	profiles     []string                          // available AWS profiles
	providers    map[string]provider.Provider      // cache: "profile/region/service" -> provider
	providersMu  sync.RWMutex
	pendingFiles map[string]*writeableSisuFile
	virtualDirs  map[string]bool
	mu           sync.RWMutex
}

// NewSisuFS creates a new SisuFS instance
func NewSisuFS(cfg Config) (*SisuFS, error) {
	fs := &SisuFS{
		FileSystem:   pathfs.NewDefaultFileSystem(),
		config:       cfg,
		providers:    make(map[string]provider.Provider),
		pendingFiles: make(map[string]*writeableSisuFile),
		virtualDirs:  make(map[string]bool),
	}

	if cfg.Regions == nil || len(cfg.Regions) == 0 {
		fs.config.Regions = defaultRegions
	}

	// Load profiles from AWS credentials/config
	profiles, err := loadAWSProfiles()
	if err != nil {
		return nil, err
	}
	fs.profiles = profiles

	return fs, nil
}

// loadAWSProfiles reads profile names from ~/.aws/credentials and ~/.aws/config
func loadAWSProfiles() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"default"}, nil
	}

	profiles := make(map[string]bool)
	profiles["default"] = true

	// Read credentials file
	credPath := filepath.Join(home, ".aws", "credentials")
	if cfg, err := ini.Load(credPath); err == nil {
		for _, section := range cfg.Sections() {
			name := section.Name()
			if name != "DEFAULT" {
				profiles[name] = true
			}
		}
	}

	// Read config file
	configPath := filepath.Join(home, ".aws", "config")
	if cfg, err := ini.Load(configPath); err == nil {
		for _, section := range cfg.Sections() {
			name := section.Name()
			if name != "DEFAULT" {
				// Config file uses "profile xxx" format
				name = strings.TrimPrefix(name, "profile ")
				profiles[name] = true
			}
		}
	}

	result := make([]string, 0, len(profiles))
	for p := range profiles {
		result = append(result, p)
	}
	return result, nil
}

// getProvider returns a cached provider or creates a new one
func (f *SisuFS) getProvider(profile, region, service string) (provider.Provider, error) {
	key := profile + "/" + region + "/" + service

	f.providersMu.RLock()
	if p, ok := f.providers[key]; ok {
		f.providersMu.RUnlock()
		return p, nil
	}
	f.providersMu.RUnlock()

	f.providersMu.Lock()
	defer f.providersMu.Unlock()

	// Double-check after acquiring write lock
	if p, ok := f.providers[key]; ok {
		return p, nil
	}

	// Use "default" if profile is "default"
	profileArg := profile
	if profile == "default" {
		profileArg = ""
	}

	var p provider.Provider
	var err error

	switch service {
	case "s3":
		p, err = provider.NewS3Provider(profileArg, region)
	case "ssm":
		p, err = provider.NewSSMProvider(profileArg, region)
	case "vpc":
		p, err = provider.NewVPCProvider(profileArg, region)
	case "iam":
		p, err = provider.NewIAMProvider(profileArg, region)
	case "lambda":
		p, err = provider.NewLambdaProvider(profileArg, region)
	case "ec2":
		p, err = provider.NewEC2Provider(profileArg, region)
	default:
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	f.providers[key] = p
	return p, nil
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

// parsePath parses a path and returns profile, region, service, and subpath
// Structure: profile/region/service/subpath or profile/global/service/subpath
func (f *SisuFS) parsePath(path string) (profile, region, service, subpath string, ok bool) {
	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 1 {
		return "", "", "", "", false
	}

	profile = parts[0]
	if len(parts) < 2 {
		return profile, "", "", "", true
	}

	region = parts[1]
	if len(parts) < 3 {
		return profile, region, "", "", true
	}

	service = parts[2]
	if len(parts) < 4 {
		return profile, region, service, "", true
	}

	subpath = parts[3]
	return profile, region, service, subpath, true
}

// GetAttr returns file attributes
func (f *SisuFS) GetAttr(name string, ctx *fuse.Context) (*fuse.Attr, fuse.Status) {
	if Debug {
		log.Printf("[fs] GetAttr: name=%q", name)
	}

	// Root directory
	if name == "" {
		return &fuse.Attr{Mode: fuse.S_IFDIR | 0777}, fuse.OK
	}

	// Quick reject for shell probe files
	baseName := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		baseName = name[idx+1:]
	}
	if ignoredFiles[baseName] {
		return nil, fuse.ENOENT
	}

	profile, region, service, subpath, ok := f.parsePath(name)
	if !ok {
		return nil, fuse.ENOENT
	}

	// Check pending files and virtual dirs
	f.mu.RLock()
	if pending, ok := f.pendingFiles[name]; ok {
		f.mu.RUnlock()
		return &fuse.Attr{Mode: fuse.S_IFREG | 0666, Size: uint64(pending.buf.Len())}, fuse.OK
	}
	if f.virtualDirs[name] {
		f.mu.RUnlock()
		return &fuse.Attr{Mode: fuse.S_IFDIR | 0777}, fuse.OK
	}
	f.mu.RUnlock()

	// Profile level
	if region == "" {
		for _, p := range f.profiles {
			if p == profile {
				return &fuse.Attr{Mode: fuse.S_IFDIR | 0555}, fuse.OK
			}
		}
		return nil, fuse.ENOENT
	}

	// Region/global level
	if service == "" {
		if region == "global" {
			return &fuse.Attr{Mode: fuse.S_IFDIR | 0555}, fuse.OK
		}
		for _, r := range f.config.Regions {
			if r == region {
				return &fuse.Attr{Mode: fuse.S_IFDIR | 0555}, fuse.OK
			}
		}
		return nil, fuse.ENOENT
	}

	// Service level
	if subpath == "" {
		mode := uint32(0555) // read-only by default
		if writableServices[service] {
			mode = 0755
		}
		if region == "global" && globalServices[service] {
			return &fuse.Attr{Mode: fuse.S_IFDIR | mode}, fuse.OK
		}
		for _, s := range regionalServices {
			if s == service {
				return &fuse.Attr{Mode: fuse.S_IFDIR | mode}, fuse.OK
			}
		}
		return nil, fuse.ENOENT
	}

	// Delegate to provider
	actualRegion := region
	if region == "global" {
		actualRegion = "us-east-1" // IAM/S3 default
	}

	prov, err := f.getProvider(profile, actualRegion, service)
	if err != nil || prov == nil {
		return nil, fuse.ENOENT
	}

	entry, err := prov.Stat(context.Background(), subpath)
	if err != nil {
		return nil, fuse.ENOENT
	}

	attr := &fuse.Attr{
		Size:  uint64(entry.Size),
		Mtime: uint64(entry.ModTime.Unix()),
	}

	if entry.IsDir {
		if writableServices[service] {
			attr.Mode = fuse.S_IFDIR | 0755
		} else {
			attr.Mode = fuse.S_IFDIR | 0555
		}
	} else {
		if writableServices[service] {
			attr.Mode = fuse.S_IFREG | 0644
		} else {
			attr.Mode = fuse.S_IFREG | 0444
		}
	}

	return attr, fuse.OK
}

// Access checks file access permissions
func (f *SisuFS) Access(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	return fuse.OK
}

// Mkdir creates a directory
func (f *SisuFS) Mkdir(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	if Debug {
		log.Printf("[fs] Mkdir: name=%q mode=%d", name, mode)
	}

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

	profile, region, service, subpath, ok := f.parsePath(name)
	if !ok || subpath == "" {
		return fuse.EPERM
	}

	actualRegion := region
	if region == "global" {
		actualRegion = "us-east-1"
	}

	prov, err := f.getProvider(profile, actualRegion, service)
	if err != nil || prov == nil {
		return fuse.ENOENT
	}

	if err := prov.Delete(context.Background(), subpath); err != nil {
		return fuse.EIO
	}

	return fuse.OK
}

// OpenDir opens a directory for reading
func (f *SisuFS) OpenDir(name string, ctx *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	if Debug {
		log.Printf("[fs] OpenDir: name=%q", name)
	}

	// Root directory - list profiles
	if name == "" {
		entries := make([]fuse.DirEntry, len(f.profiles))
		for i, p := range f.profiles {
			entries[i] = fuse.DirEntry{Name: p, Mode: fuse.S_IFDIR | 0555}
		}
		return entries, fuse.OK
	}

	profile, region, service, subpath, ok := f.parsePath(name)
	if !ok {
		return nil, fuse.ENOENT
	}

	// Profile level: list regions + global
	if region == "" {
		entries := make([]fuse.DirEntry, 0, len(f.config.Regions)+1)
		entries = append(entries, fuse.DirEntry{Name: "global", Mode: fuse.S_IFDIR | 0555})
		for _, r := range f.config.Regions {
			entries = append(entries, fuse.DirEntry{Name: r, Mode: fuse.S_IFDIR | 0555})
		}
		return entries, fuse.OK
	}

	// Region/global level: list services
	if service == "" {
		var services []string
		if region == "global" {
			for s := range globalServices {
				services = append(services, s)
			}
		} else {
			services = regionalServices
		}
		entries := make([]fuse.DirEntry, len(services))
		for i, s := range services {
			mode := uint32(0555)
			if writableServices[s] {
				mode = 0755
			}
			entries[i] = fuse.DirEntry{Name: s, Mode: fuse.S_IFDIR | mode}
		}
		return entries, fuse.OK
	}

	// Service level: delegate to provider
	actualRegion := region
	if region == "global" {
		actualRegion = "us-east-1"
	}

	prov, err := f.getProvider(profile, actualRegion, service)
	if err != nil || prov == nil {
		// Check virtual directory
		f.mu.RLock()
		isVirtual := f.virtualDirs[name]
		f.mu.RUnlock()
		if isVirtual {
			return []fuse.DirEntry{}, fuse.OK
		}
		return nil, fuse.ENOENT
	}

	provEntries, err := prov.ReadDir(context.Background(), subpath)
	if err != nil {
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
		var mode uint32
		if e.IsDir {
			if writableServices[service] {
				mode = fuse.S_IFDIR | 0755
			} else {
				mode = fuse.S_IFDIR | 0555
			}
		} else {
			if writableServices[service] {
				mode = fuse.S_IFREG | 0644
			} else {
				mode = fuse.S_IFREG | 0444
			}
		}
		entries[i] = fuse.DirEntry{Name: e.Name, Mode: mode}
	}

	return entries, fuse.OK
}

// Open opens a file for reading
func (f *SisuFS) Open(name string, flags uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	if Debug {
		log.Printf("[fs] Open: name=%q flags=%d", name, flags)
	}

	profile, region, service, subpath, ok := f.parsePath(name)
	if !ok || subpath == "" {
		return nil, fuse.ENOENT
	}

	actualRegion := region
	if region == "global" {
		actualRegion = "us-east-1"
	}

	prov, err := f.getProvider(profile, actualRegion, service)
	if err != nil || prov == nil {
		return nil, fuse.ENOENT
	}

	data, err := prov.Read(context.Background(), subpath)
	if err != nil {
		if Debug {
			log.Printf("[fs] Open: Read failed for %q: %v", name, err)
		}
		return nil, fuse.EIO
	}

	return &sisuFile{File: nodefs.NewDefaultFile(), data: data}, fuse.OK
}

// Create creates a new file for writing
func (f *SisuFS) Create(name string, flags uint32, mode uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	if Debug {
		log.Printf("[fs] Create: name=%q flags=%d mode=%d", name, flags, mode)
	}

	profile, region, service, subpath, ok := f.parsePath(name)
	if !ok || subpath == "" {
		return nil, fuse.EPERM
	}

	actualRegion := region
	if region == "global" {
		actualRegion = "us-east-1"
	}

	prov, err := f.getProvider(profile, actualRegion, service)
	if err != nil || prov == nil {
		return nil, fuse.ENOENT
	}

	wf := &writeableSisuFile{
		File: nodefs.NewDefaultFile(),
		prov: prov,
		path: subpath,
		fs:   f,
		name: name,
	}

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

func (f *sisuFile) Release()                          {}
func (f *sisuFile) Flush() fuse.Status                { return fuse.OK }
func (f *sisuFile) Fsync(flags int) fuse.Status       { return fuse.OK }
func (f *sisuFile) Truncate(size uint64) fuse.Status  { return fuse.Status(syscall.EROFS) }
func (f *sisuFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	return 0, fuse.Status(syscall.EROFS)
}

// writeableSisuFile is a file that buffers writes and flushes to provider
type writeableSisuFile struct {
	nodefs.File
	prov provider.Provider
	path string
	buf  bytes.Buffer
	fs   *SisuFS
	name string
}

func (f *writeableSisuFile) Write(data []byte, off int64) (uint32, fuse.Status) {
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
	if f.buf.Len() == 0 {
		return fuse.OK
	}
	if err := f.prov.Write(context.Background(), f.path, f.buf.Bytes()); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (f *writeableSisuFile) Release() {
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
