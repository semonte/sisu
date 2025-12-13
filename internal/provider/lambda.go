package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/semonte/sisu/internal/cache"
)

// LambdaProvider provides access to AWS Lambda functions
type LambdaProvider struct {
	ReadOnlyProvider
	client *lambda.Client
	cache  *cache.Cache
}

// NewLambdaProvider creates a new Lambda provider
func NewLambdaProvider(profile, region string) (*LambdaProvider, error) {
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

	return &LambdaProvider{
		client: lambda.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *LambdaProvider) Name() string {
	return "lambda"
}

func (p *LambdaProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *LambdaProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list all functions
	if path == "" {
		return p.listFunctions(ctx)
	}

	// Function directory: show files
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return []Entry{
			{Name: "config.json", IsDir: false},
			{Name: "policy.json", IsDir: false},
			{Name: "env.json", IsDir: false},
		}, nil
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *LambdaProvider) listFunctions(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	var marker *string

	for {
		resp, err := p.client.ListFunctions(ctx, &lambda.ListFunctionsInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}

		for _, fn := range resp.Functions {
			entries = append(entries, Entry{
				Name:  aws.ToString(fn.FunctionName),
				IsDir: true,
			})
		}

		if resp.NextMarker == nil {
			break
		}
		marker = resp.NextMarker
	}

	return entries, nil
}

func (p *LambdaProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *LambdaProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	functionName := parts[0]
	file := parts[1]

	switch file {
	case "config.json":
		return p.getFunctionConfig(ctx, functionName)
	case "policy.json":
		return p.getFunctionPolicy(ctx, functionName)
	case "env.json":
		return p.getFunctionEnv(ctx, functionName)
	}

	return nil, fmt.Errorf("unknown file: %s", file)
}

func (p *LambdaProvider) getFunctionConfig(ctx context.Context, functionName string) ([]byte, error) {
	resp, err := p.client.GetFunction(ctx, &lambda.GetFunctionInput{
		FunctionName: aws.String(functionName),
	})
	if err != nil {
		return nil, err
	}

	return json.MarshalIndent(resp.Configuration, "", "  ")
}

func (p *LambdaProvider) getFunctionPolicy(ctx context.Context, functionName string) ([]byte, error) {
	resp, err := p.client.GetPolicy(ctx, &lambda.GetPolicyInput{
		FunctionName: aws.String(functionName),
	})
	if err != nil {
		// No policy is common, return empty object
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return []byte("{}"), nil
		}
		return nil, err
	}

	// Policy is returned as a JSON string, parse and re-indent
	var policy interface{}
	if err := json.Unmarshal([]byte(aws.ToString(resp.Policy)), &policy); err != nil {
		return []byte(aws.ToString(resp.Policy)), nil
	}

	return json.MarshalIndent(policy, "", "  ")
}

func (p *LambdaProvider) getFunctionEnv(ctx context.Context, functionName string) ([]byte, error) {
	resp, err := p.client.GetFunction(ctx, &lambda.GetFunctionInput{
		FunctionName: aws.String(functionName),
	})
	if err != nil {
		return nil, err
	}

	env := make(map[string]string)
	if resp.Configuration.Environment != nil && resp.Configuration.Environment.Variables != nil {
		env = resp.Configuration.Environment.Variables
	}

	return json.MarshalIndent(env, "", "  ")
}

func (p *LambdaProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *LambdaProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "lambda", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")

	// Function directory
	if len(parts) == 1 {
		_, err := p.client.GetFunction(ctx, &lambda.GetFunctionInput{
			FunctionName: aws.String(parts[0]),
		})
		if err != nil {
			return nil, fmt.Errorf("function not found: %s", parts[0])
		}
		return &Entry{Name: parts[0], IsDir: true}, nil
	}

	// Files
	if len(parts) == 2 {
		switch parts[1] {
		case "config.json", "policy.json", "env.json":
			return &Entry{Name: parts[1], IsDir: false, Size: 4096}, nil
		}
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
