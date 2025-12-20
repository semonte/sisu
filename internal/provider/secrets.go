package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/semonte/sisu/internal/cache"
)

// SecretsProvider provides access to AWS Secrets Manager
type SecretsProvider struct {
	ReadOnlyProvider
	client *secretsmanager.Client
	cache  *cache.Cache
}

// NewSecretsProvider creates a new Secrets Manager provider
func NewSecretsProvider(profile, region string) (*SecretsProvider, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	return &SecretsProvider{
		client: secretsmanager.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *SecretsProvider) Name() string {
	return "secrets"
}

func (p *SecretsProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	cacheKey := "readdir:" + path
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]Entry), nil
	}

	entries, err := p.readDirUncached(ctx, path)
	if err == nil {
		p.cache.Set(cacheKey, entries)
	}
	return entries, err
}

func (p *SecretsProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Get all secrets and filter by path prefix
	allSecrets, err := p.listAllSecrets(ctx)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	seen := make(map[string]bool)

	prefix := path
	if prefix != "" {
		prefix += "/"
	}

	for _, secretName := range allSecrets {
		// Check if this secret is under the current path
		if !strings.HasPrefix(secretName, prefix) && path != "" {
			continue
		}

		// Get the relative name
		relName := secretName
		if prefix != "" {
			relName = strings.TrimPrefix(secretName, prefix)
		}

		if relName == "" {
			continue
		}

		// Check if this is a "directory" (has more path components)
		if idx := strings.Index(relName, "/"); idx >= 0 {
			dirName := relName[:idx]
			if !seen[dirName] {
				seen[dirName] = true
				entries = append(entries, Entry{
					Name:  dirName,
					IsDir: true,
				})
			}
		} else {
			// It's a secret (directory with info.json and value)
			entries = append(entries, Entry{
				Name:  relName,
				IsDir: true,
			})
		}
	}

	// If path points to an actual secret, show its files
	for _, secretName := range allSecrets {
		if secretName == path {
			return []Entry{
				{Name: "info.json", IsDir: false},
				{Name: "value", IsDir: false},
			}, nil
		}
	}

	return entries, nil
}

func (p *SecretsProvider) listAllSecrets(ctx context.Context) ([]string, error) {
	cacheKey := "allsecrets"
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]string), nil
	}

	var secrets []string
	var nextToken *string

	for {
		resp, err := p.client.ListSecrets(ctx, &secretsmanager.ListSecretsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}

		for _, secret := range resp.SecretList {
			secrets = append(secrets, aws.ToString(secret.Name))
		}

		if resp.NextToken == nil {
			break
		}
		nextToken = resp.NextToken
	}

	p.cache.Set(cacheKey, secrets)
	return secrets, nil
}

func (p *SecretsProvider) Read(ctx context.Context, path string) ([]byte, error) {
	cacheKey := "read:" + path
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]byte), nil
	}

	data, err := p.readUncached(ctx, path)
	if err == nil {
		p.cache.Set(cacheKey, data)
	}
	return data, err
}

func (p *SecretsProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	// Path format: secret/name/with/slashes/info.json or secret/name/with/slashes/value
	// We need to find where the secret name ends and the file name begins

	if strings.HasSuffix(path, "/info.json") {
		secretName := strings.TrimSuffix(path, "/info.json")
		return p.getSecretInfo(ctx, secretName)
	}

	if strings.HasSuffix(path, "/value") {
		secretName := strings.TrimSuffix(path, "/value")
		return p.getSecretValue(ctx, secretName)
	}

	return nil, fmt.Errorf("unknown file: %s", path)
}

func (p *SecretsProvider) getSecretInfo(ctx context.Context, secretName string) ([]byte, error) {
	resp, err := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return nil, err
	}

	return json.MarshalIndent(resp, "", "  ")
}

func (p *SecretsProvider) getSecretValue(ctx context.Context, secretName string) ([]byte, error) {
	resp, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return nil, err
	}

	// Secret can be string or binary
	if resp.SecretString != nil {
		// Try to pretty print if it's JSON
		var jsonData interface{}
		if err := json.Unmarshal([]byte(*resp.SecretString), &jsonData); err == nil {
			return json.MarshalIndent(jsonData, "", "  ")
		}
		return []byte(*resp.SecretString), nil
	}

	if resp.SecretBinary != nil {
		return resp.SecretBinary, nil
	}

	return []byte{}, nil
}

func (p *SecretsProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *SecretsProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "secrets", IsDir: true}, nil
	}

	// Check if it's a file (info.json or value)
	if strings.HasSuffix(path, "/info.json") || strings.HasSuffix(path, "/value") {
		parts := strings.Split(path, "/")
		return &Entry{Name: parts[len(parts)-1], IsDir: false, Size: 4096}, nil
	}

	// Check if it's an exact secret match
	allSecrets, err := p.listAllSecrets(ctx)
	if err != nil {
		return nil, err
	}

	for _, secretName := range allSecrets {
		if secretName == path {
			// It's an actual secret - show as directory
			return &Entry{Name: path, IsDir: true}, nil
		}
	}

	// Check if it's a prefix (virtual directory)
	prefix := path + "/"
	for _, secretName := range allSecrets {
		if strings.HasPrefix(secretName, prefix) {
			return &Entry{Name: path, IsDir: true}, nil
		}
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
