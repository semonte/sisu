package provider

import (
	"context"
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
