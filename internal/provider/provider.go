package provider

import (
	"context"
	"io"
	"io/fs"
	"time"
)

// Entry represents a file or directory entry
type Entry struct {
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// StreamingFile represents a file that can be read incrementally
type StreamingFile interface {
	io.ReadCloser
	// Size returns the current known size (-1 if unknown/streaming)
	Size() int64
}

// StreamingProvider is implemented by providers that support streaming file reads
type StreamingProvider interface {
	// OpenStream opens a file for streaming read. Returns nil if not streamable.
	OpenStream(ctx context.Context, path string) (StreamingFile, error)
}

// Provider defines the interface for AWS resource providers
type Provider interface {
	// Name returns the provider name (e.g., "s3", "dynamodb")
	Name() string

	// ReadDir lists entries at the given path
	ReadDir(ctx context.Context, path string) ([]Entry, error)

	// Read returns the content of a file at the given path
	Read(ctx context.Context, path string) ([]byte, error)

	// Stat returns info about a single entry
	Stat(ctx context.Context, path string) (*Entry, error)

	// Write writes content to a file (optional, can return fs.ErrPermission)
	Write(ctx context.Context, path string, data []byte) error

	// Delete removes a file (optional, can return fs.ErrPermission)
	Delete(ctx context.Context, path string) error
}

// ReadOnlyProvider provides a base implementation that returns permission errors for writes
type ReadOnlyProvider struct{}

func (p *ReadOnlyProvider) Write(ctx context.Context, path string, data []byte) error {
	return fs.ErrPermission
}

func (p *ReadOnlyProvider) Delete(ctx context.Context, path string) error {
	return fs.ErrPermission
}
