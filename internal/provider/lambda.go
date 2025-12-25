package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/semonte/sisu/internal/cache"
)

// LambdaProvider provides access to AWS Lambda functions
type LambdaProvider struct {
	ReadOnlyProvider
	client     *lambda.Client
	logsClient *cloudwatchlogs.Client
	cache      *cache.Cache
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
		client:     lambda.NewFromConfig(cfg),
		logsClient: cloudwatchlogs.NewFromConfig(cfg),
		cache:      cache.New(5 * time.Minute),
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

	parts := strings.SplitN(path, "/", 2)
	functionName := parts[0]

	// Function directory: show files
	if len(parts) == 1 {
		return []Entry{
			{Name: "config.json", IsDir: false},
			{Name: "policy.json", IsDir: false},
			{Name: "env.json", IsDir: false},
			{Name: "logs", IsDir: true},
		}, nil
	}

	// logs/ directory
	subPath := parts[1]
	if subPath == "logs" {
		// Check if log group exists
		logGroupName := "/aws/lambda/" + functionName
		_, err := p.logsClient.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: aws.String(logGroupName),
			Limit:              aws.Int32(1),
		})
		if err != nil {
			return []Entry{}, nil // No logs available
		}
		return []Entry{
			{Name: "latest.log", IsDir: false},
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
	if len(parts) < 2 {
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
	case "logs":
		if len(parts) == 3 && parts[2] == "latest.log" {
			return p.getLatestLogs(ctx, functionName)
		}
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

func (p *LambdaProvider) getLatestLogs(ctx context.Context, functionName string) ([]byte, error) {
	logGroupName := "/aws/lambda/" + functionName

	// Get events from the last hour
	startTime := time.Now().Add(-1 * time.Hour).UnixMilli()

	resp, err := p.logsClient.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(logGroupName),
		StartTime:    aws.Int64(startTime),
		Limit:        aws.Int32(500),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get logs: %w", err)
	}

	var buf strings.Builder
	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		buf.WriteString(fmt.Sprintf("[%s] %s", ts, msg))
	}

	if buf.Len() == 0 {
		return []byte("# No events in the last hour\n"), nil
	}

	return []byte(buf.String()), nil
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

	// Files and directories under function
	if len(parts) == 2 {
		switch parts[1] {
		case "config.json", "policy.json", "env.json":
			return &Entry{Name: parts[1], IsDir: false, Size: 4096}, nil
		case "logs":
			return &Entry{Name: "logs", IsDir: true}, nil
		}
	}

	// logs/latest.log
	if len(parts) == 3 && parts[1] == "logs" && parts[2] == "latest.log" {
		return &Entry{Name: "latest.log", IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// streamingLambdaLogFile implements StreamingFile for Lambda logs
type streamingLambdaLogFile struct {
	client       *cloudwatchlogs.Client
	logGroupName string
	buffer       []byte
	readOffset   int
	nextToken    *string
	fullyLoaded  bool
	ctx          context.Context
}

// OpenStream opens a streaming file for logs/latest.log
func (p *LambdaProvider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	// Only stream logs/latest.log files
	if !strings.HasSuffix(path, "/logs/latest.log") {
		return nil, nil
	}

	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return nil, nil
	}

	functionName := parts[0]
	logGroupName := "/aws/lambda/" + functionName

	return &streamingLambdaLogFile{
		client:       p.logsClient,
		logGroupName: logGroupName,
		ctx:          ctx,
	}, nil
}

func (f *streamingLambdaLogFile) Read(p []byte) (int, error) {
	// If we need more data and haven't loaded everything, fetch more
	for f.readOffset >= len(f.buffer) && !f.fullyLoaded {
		if err := f.fetchNextPage(); err != nil {
			return 0, err
		}
	}

	// EOF - fully loaded and nothing left to read
	if f.readOffset >= len(f.buffer) {
		return 0, io.EOF
	}

	n := copy(p, f.buffer[f.readOffset:])
	f.readOffset += n

	// If we've read everything and fully loaded, signal EOF with the data
	if f.readOffset >= len(f.buffer) && f.fullyLoaded {
		return n, io.EOF
	}

	return n, nil
}

func (f *streamingLambdaLogFile) fetchNextPage() error {
	isFirstPage := f.nextToken == nil

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(f.logGroupName),
		Limit:        aws.Int32(100),
	}
	if f.nextToken != nil {
		input.NextToken = f.nextToken
	} else {
		input.StartTime = aws.Int64(time.Now().Add(-1 * time.Hour).UnixMilli())
	}

	resp, err := f.client.FilterLogEvents(f.ctx, input)
	if err != nil {
		return err
	}

	if isFirstPage && len(f.buffer) == 0 {
		f.buffer = append(f.buffer, []byte(fmt.Sprintf("# Log group: %s\n\n", f.logGroupName))...)
	}

	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		f.buffer = append(f.buffer, []byte(fmt.Sprintf("[%s] %s", ts, msg))...)
	}

	f.nextToken = resp.NextToken
	if f.nextToken == nil {
		f.fullyLoaded = true
		if isFirstPage && len(resp.Events) == 0 {
			f.buffer = append(f.buffer, []byte("# No events in the last hour\n")...)
		}
	}

	return nil
}

func (f *streamingLambdaLogFile) Close() error {
	f.buffer = nil
	return nil
}

func (f *streamingLambdaLogFile) Size() int64 {
	return -1 // Unknown size for streaming
}
