package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/semonte/sisu/internal/cache"
)

// S3Provider provides access to S3 buckets and objects
type S3Provider struct {
	ReadOnlyProvider
	client *s3.Client
	cache  *cache.Cache
}

// NewS3Provider creates a new S3 provider
func NewS3Provider(profile, region string) (*S3Provider, error) {
	var opts []func(*config.LoadOptions) error

	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &S3Provider{
		client: s3.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *S3Provider) Name() string {
	return "s3"
}

func (p *S3Provider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	cacheKey := "readdir:" + path
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]Entry), nil
	}

	var entries []Entry
	var err error

	// Root of S3 - list buckets
	if path == "" {
		entries, err = p.listBuckets(ctx)
	} else {
		// Inside a bucket - list objects
		parts := strings.SplitN(path, "/", 2)
		bucket := parts[0]
		prefix := ""
		if len(parts) > 1 {
			prefix = parts[1]
			if prefix != "" && !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
		}
		entries, err = p.listObjects(ctx, bucket, prefix)
	}

	if err == nil {
		p.cache.Set(cacheKey, entries)
	}
	return entries, err
}

func (p *S3Provider) listBuckets(ctx context.Context) ([]Entry, error) {
	resp, err := p.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(resp.Buckets))
	for i, bucket := range resp.Buckets {
		modTime := time.Time{}
		if bucket.CreationDate != nil {
			modTime = *bucket.CreationDate
		}
		entries[i] = Entry{
			Name:    *bucket.Name,
			IsDir:   true,
			ModTime: modTime,
		}
	}

	return entries, nil
}

const maxS3Entries = 100

func (p *S3Provider) listObjects(ctx context.Context, bucket, prefix string) ([]Entry, error) {
	var entries []Entry
	truncated := false

	resp, err := p.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(maxS3Entries),
	})
	if err != nil {
		return nil, err
	}

	// Add "directories" (common prefixes)
	for _, cp := range resp.CommonPrefixes {
		name := strings.TrimPrefix(*cp.Prefix, prefix)
		name = strings.TrimSuffix(name, "/")
		if name != "" {
			entries = append(entries, Entry{
				Name:  name,
				IsDir: true,
			})
		}
	}

	// Add files (objects)
	for _, obj := range resp.Contents {
		name := strings.TrimPrefix(*obj.Key, prefix)
		if name != "" && name != "/" {
			modTime := time.Time{}
			if obj.LastModified != nil {
				modTime = *obj.LastModified
			}
			entries = append(entries, Entry{
				Name:    name,
				IsDir:   false,
				Size:    *obj.Size,
				ModTime: modTime,
			})
		}
	}

	if resp.IsTruncated != nil && *resp.IsTruncated {
		truncated = true
	}

	// Add indicator file if there are more results
	if truncated {
		entries = append(entries, Entry{
			Name:  "_more_results.txt",
			IsDir: false,
			Size:  int64(len(moreResultsMessage(len(entries)))),
		})
	}

	return entries, nil
}

func moreResultsMessage(shown int) string {
	return fmt.Sprintf("Showing first %d entries. There are more results not displayed.\n"+
		"Use AWS CLI for full listing: aws s3 ls s3://bucket/prefix/\n", shown)
}

func (p *S3Provider) Read(ctx context.Context, path string) ([]byte, error) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	bucket := parts[0]
	key := parts[1]

	// Handle virtual _more_results.txt file
	if strings.HasSuffix(key, "_more_results.txt") {
		return []byte(moreResultsMessage(maxS3Entries)), nil
	}

	resp, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (p *S3Provider) Stat(ctx context.Context, path string) (*Entry, error) {
	cacheKey := "stat:" + path
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(*Entry), nil
	}

	entry, err := p.statUncached(ctx, path)
	if err == nil {
		p.cache.Set(cacheKey, entry)
	}
	return entry, err
}

func (p *S3Provider) statUncached(ctx context.Context, path string) (*Entry, error) {
	parts := strings.SplitN(path, "/", 2)
	bucket := parts[0]

	// Just a bucket name - it's a directory
	if len(parts) == 1 {
		// Verify bucket exists
		_, err := p.client.HeadBucket(ctx, &s3.HeadBucketInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			return nil, err
		}
		return &Entry{
			Name:  bucket,
			IsDir: true,
		}, nil
	}

	key := parts[1]

	// Handle virtual _more_results.txt file
	if strings.HasSuffix(key, "_more_results.txt") {
		return &Entry{
			Name:  "_more_results.txt",
			IsDir: false,
			Size:  int64(len(moreResultsMessage(maxS3Entries))),
		}, nil
	}

	// Check if it's a "directory" (prefix with objects under it)
	listResp, err := p.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(key + "/"),
		MaxKeys: aws.Int32(1),
	})
	if err == nil && (len(listResp.Contents) > 0 || len(listResp.CommonPrefixes) > 0) {
		return &Entry{
			Name:  key,
			IsDir: true,
		}, nil
	}

	// Try to get object metadata
	resp, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}

	modTime := time.Time{}
	if resp.LastModified != nil {
		modTime = *resp.LastModified
	}

	size := int64(0)
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}

	return &Entry{
		Name:    key,
		IsDir:   false,
		Size:    size,
		ModTime: modTime,
	}, nil
}

func (p *S3Provider) Write(ctx context.Context, path string, data []byte) error {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid path: %s", path)
	}

	bucket := parts[0]
	key := parts[1]

	_, err := p.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return err
	}

	// Invalidate cache for parent directory
	p.invalidateCache(path, parts[0])

	return nil
}

func (p *S3Provider) Delete(ctx context.Context, path string) error {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid path: %s", path)
	}

	bucket := parts[0]
	key := parts[1]

	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}

	// Invalidate cache
	p.invalidateCache(path, bucket)

	return nil
}

func (p *S3Provider) invalidateCache(path, bucket string) {
	parentPath := path
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		parentPath = path[:idx]
	} else {
		parentPath = bucket
	}
	p.cache.Delete("readdir:" + parentPath)
	p.cache.Delete("stat:" + path)
}
