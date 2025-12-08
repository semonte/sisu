package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/smonte/sisu/internal/cache"
)

// SSMProvider provides access to SSM Parameter Store
type SSMProvider struct {
	client *ssm.Client
	cache  *cache.Cache
}

// NewSSMProvider creates a new SSM provider
func NewSSMProvider(profile, region string) (*SSMProvider, error) {
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

	return &SSMProvider{
		client: ssm.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *SSMProvider) Name() string {
	return "ssm"
}

func (p *SSMProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	cacheKey := "readdir:" + path
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]Entry), nil
	}

	// SSM paths must start with /
	ssmPath := "/" + path
	if ssmPath == "/" {
		ssmPath = "/"
	}
	if !strings.HasSuffix(ssmPath, "/") {
		ssmPath += "/"
	}

	entries, err := p.listParameters(ctx, ssmPath)
	if err != nil {
		return nil, err
	}

	p.cache.Set(cacheKey, entries)
	return entries, nil
}

func (p *SSMProvider) listParameters(ctx context.Context, path string) ([]Entry, error) {
	var entries []Entry
	seen := make(map[string]bool)

	// Use GetParametersByPath to list parameters under this path
	paginator := ssm.NewGetParametersByPathPaginator(p.client, &ssm.GetParametersByPathInput{
		Path:           aws.String(path),
		Recursive:      aws.Bool(false),
		WithDecryption: aws.Bool(false),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			// If path doesn't exist, return empty
			return entries, nil
		}

		for _, param := range page.Parameters {
			name := strings.TrimPrefix(*param.Name, path)
			name = strings.TrimPrefix(name, "/")

			// Skip if empty
			if name == "" {
				continue
			}

			// Check if this is a "directory" (has more path components)
			if idx := strings.Index(name, "/"); idx >= 0 {
				dirName := name[:idx]
				if !seen[dirName] {
					seen[dirName] = true
					entries = append(entries, Entry{
						Name:  dirName,
						IsDir: true,
					})
				}
			} else {
				// It's a parameter (file)
				modTime := time.Time{}
				if param.LastModifiedDate != nil {
					modTime = *param.LastModifiedDate
				}
				entries = append(entries, Entry{
					Name:    name,
					IsDir:   false,
					Size:    int64(len(aws.ToString(param.Value))),
					ModTime: modTime,
				})
			}
		}
	}

	// Also check for "subdirectories" by looking for parameters with this prefix
	// Use DescribeParameters to find paths that might be directories
	descPaginator := ssm.NewDescribeParametersPaginator(p.client, &ssm.DescribeParametersInput{
		ParameterFilters: []types.ParameterStringFilter{
			{
				Key:    aws.String("Path"),
				Option: aws.String("Recursive"),
				Values: []string{path},
			},
		},
	})

	for descPaginator.HasMorePages() {
		page, err := descPaginator.NextPage(ctx)
		if err != nil {
			break
		}

		for _, param := range page.Parameters {
			name := strings.TrimPrefix(*param.Name, path)
			name = strings.TrimPrefix(name, "/")

			if name == "" {
				continue
			}

			// Check if this creates a directory entry
			if idx := strings.Index(name, "/"); idx >= 0 {
				dirName := name[:idx]
				if !seen[dirName] {
					seen[dirName] = true
					entries = append(entries, Entry{
						Name:  dirName,
						IsDir: true,
					})
				}
			}
		}
	}

	return entries, nil
}

func (p *SSMProvider) Read(ctx context.Context, path string) ([]byte, error) {
	ssmPath := "/" + path

	resp, err := p.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(ssmPath),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}

	value := aws.ToString(resp.Parameter.Value)
	// Add newline for better cat output
	if !strings.HasSuffix(value, "\n") {
		value += "\n"
	}

	return []byte(value), nil
}

func (p *SSMProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *SSMProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "ssm", IsDir: true}, nil
	}

	ssmPath := "/" + path

	// First, try to get it as a parameter
	resp, err := p.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(ssmPath),
		WithDecryption: aws.Bool(false),
	})
	if err == nil {
		modTime := time.Time{}
		if resp.Parameter.LastModifiedDate != nil {
			modTime = *resp.Parameter.LastModifiedDate
		}
		return &Entry{
			Name:    path,
			IsDir:   false,
			Size:    int64(len(aws.ToString(resp.Parameter.Value))),
			ModTime: modTime,
		}, nil
	}

	// Check if it's a "directory" (path prefix with children)
	checkPath := ssmPath
	if !strings.HasSuffix(checkPath, "/") {
		checkPath += "/"
	}

	listResp, err := p.client.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path:       aws.String(checkPath),
		MaxResults: aws.Int32(1),
	})
	if err == nil && len(listResp.Parameters) > 0 {
		return &Entry{
			Name:  path,
			IsDir: true,
		}, nil
	}

	return nil, fmt.Errorf("parameter not found: %s", path)
}

func (p *SSMProvider) Write(ctx context.Context, path string, data []byte) error {
	ssmPath := "/" + path
	value := strings.TrimSuffix(string(data), "\n")

	_, err := p.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(ssmPath),
		Value:     aws.String(value),
		Type:      types.ParameterTypeString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return err
	}

	p.invalidateCache(path)
	return nil
}

func (p *SSMProvider) Delete(ctx context.Context, path string) error {
	ssmPath := "/" + path

	_, err := p.client.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(ssmPath),
	})
	if err != nil {
		return err
	}

	p.invalidateCache(path)
	return nil
}

func (p *SSMProvider) invalidateCache(path string) {
	// Invalidate the parameter itself
	p.cache.Delete("stat:" + path)

	// Invalidate parent directory
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		p.cache.Delete("readdir:" + path[:idx])
	} else {
		p.cache.Delete("readdir:")
	}
}
