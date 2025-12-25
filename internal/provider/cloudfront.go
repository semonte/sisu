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
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/semonte/sisu/internal/cache"
)

// CloudFrontProvider provides access to AWS CloudFront distributions and functions
type CloudFrontProvider struct {
	ReadOnlyProvider
	client     *cloudfront.Client
	logsClient *cloudwatchlogs.Client
	cache      *cache.Cache
}

// NewCloudFrontProvider creates a new CloudFront provider
func NewCloudFrontProvider(profile, region string) (*CloudFrontProvider, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	// CloudFront is a global service, but we still need a region for the API
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	return &CloudFrontProvider{
		client:     cloudfront.NewFromConfig(cfg),
		logsClient: cloudwatchlogs.NewFromConfig(cfg),
		cache:      cache.New(5 * time.Minute),
	}, nil
}

func (p *CloudFrontProvider) Name() string {
	return "cloudfront"
}

func (p *CloudFrontProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *CloudFrontProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: show distributions and functions
	if path == "" {
		return []Entry{
			{Name: "distributions", IsDir: true},
			{Name: "functions", IsDir: true},
		}, nil
	}

	parts := strings.Split(path, "/")

	// distributions/
	if parts[0] == "distributions" {
		if len(parts) == 1 {
			return p.listDistributions(ctx)
		}
		if len(parts) == 2 {
			return []Entry{
				{Name: "info.json", IsDir: false},
				{Name: "origins.json", IsDir: false},
			}, nil
		}
	}

	// functions/
	if parts[0] == "functions" {
		if len(parts) == 1 {
			return p.listFunctions(ctx)
		}
		if len(parts) == 2 {
			return []Entry{
				{Name: "code.js", IsDir: false},
				{Name: "config.json", IsDir: false},
				{Name: "logs", IsDir: true},
			}, nil
		}
		if len(parts) == 3 && parts[2] == "logs" {
			return []Entry{
				{Name: "latest.log", IsDir: false},
			}, nil
		}
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *CloudFrontProvider) listDistributions(ctx context.Context) ([]Entry, error) {
	var entries []Entry

	paginator := cloudfront.NewListDistributionsPaginator(p.client, &cloudfront.ListDistributionsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		if page.DistributionList != nil {
			for _, dist := range page.DistributionList.Items {
				entries = append(entries, Entry{
					Name:  aws.ToString(dist.Id),
					IsDir: true,
				})
			}
		}
	}

	return entries, nil
}

func (p *CloudFrontProvider) listFunctions(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	seen := make(map[string]bool)

	resp, err := p.client.ListFunctions(ctx, &cloudfront.ListFunctionsInput{})
	if err != nil {
		return nil, err
	}

	if resp.FunctionList != nil {
		for _, fn := range resp.FunctionList.Items {
			name := aws.ToString(fn.Name)
			if !seen[name] {
				seen[name] = true
				entries = append(entries, Entry{
					Name:  name,
					IsDir: true,
				})
			}
		}
	}

	return entries, nil
}

func (p *CloudFrontProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *CloudFrontProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	parts := strings.Split(path, "/")

	// distributions/<id>/info.json
	if len(parts) == 3 && parts[0] == "distributions" && parts[2] == "info.json" {
		return p.getDistributionInfo(ctx, parts[1])
	}

	// distributions/<id>/origins.json
	if len(parts) == 3 && parts[0] == "distributions" && parts[2] == "origins.json" {
		return p.getDistributionOrigins(ctx, parts[1])
	}

	// functions/<name>/code.js
	if len(parts) == 3 && parts[0] == "functions" && parts[2] == "code.js" {
		return p.getFunctionCode(ctx, parts[1])
	}

	// functions/<name>/config.json
	if len(parts) == 3 && parts[0] == "functions" && parts[2] == "config.json" {
		return p.getFunctionConfig(ctx, parts[1])
	}

	// functions/<name>/logs/latest.log
	if len(parts) == 4 && parts[0] == "functions" && parts[2] == "logs" && parts[3] == "latest.log" {
		return p.getFunctionLogs(ctx, parts[1])
	}

	return nil, fmt.Errorf("unknown file: %s", path)
}

func (p *CloudFrontProvider) getDistributionInfo(ctx context.Context, distID string) ([]byte, error) {
	resp, err := p.client.GetDistribution(ctx, &cloudfront.GetDistributionInput{
		Id: aws.String(distID),
	})
	if err != nil {
		return nil, err
	}

	// Create a simplified info structure
	type DistInfo struct {
		ID          string `json:"Id"`
		DomainName  string `json:"DomainName"`
		Status      string `json:"Status"`
		Enabled     bool   `json:"Enabled"`
		Comment     string `json:"Comment,omitempty"`
		Aliases     []string `json:"Aliases,omitempty"`
		PriceClass  string `json:"PriceClass"`
	}

	dist := resp.Distribution
	info := DistInfo{
		ID:         aws.ToString(dist.Id),
		DomainName: aws.ToString(dist.DomainName),
		Status:     aws.ToString(dist.Status),
		Enabled:    aws.ToBool(dist.DistributionConfig.Enabled),
		Comment:    aws.ToString(dist.DistributionConfig.Comment),
		PriceClass: string(dist.DistributionConfig.PriceClass),
	}

	if dist.DistributionConfig.Aliases != nil {
		info.Aliases = dist.DistributionConfig.Aliases.Items
	}

	return json.MarshalIndent(info, "", "  ")
}

func (p *CloudFrontProvider) getDistributionOrigins(ctx context.Context, distID string) ([]byte, error) {
	resp, err := p.client.GetDistribution(ctx, &cloudfront.GetDistributionInput{
		Id: aws.String(distID),
	})
	if err != nil {
		return nil, err
	}

	type S3OriginConfig struct {
		OriginAccessIdentity string `json:"OriginAccessIdentity,omitempty"`
	}

	type Origin struct {
		ID                        string          `json:"Id"`
		DomainName                string          `json:"DomainName"`
		OriginPath                string          `json:"OriginPath,omitempty"`
		OriginAccessControlId     string          `json:"OriginAccessControlId,omitempty"`
		S3OriginConfig            *S3OriginConfig `json:"S3OriginConfig,omitempty"`
		OriginAccessConfigWarning string          `json:"_warning,omitempty"`
	}

	var origins []Origin
	if resp.Distribution.DistributionConfig.Origins != nil {
		for _, o := range resp.Distribution.DistributionConfig.Origins.Items {
			origin := Origin{
				ID:                    aws.ToString(o.Id),
				DomainName:            aws.ToString(o.DomainName),
				OriginPath:            aws.ToString(o.OriginPath),
				OriginAccessControlId: aws.ToString(o.OriginAccessControlId),
			}

			// Check for legacy OAI config
			if o.S3OriginConfig != nil && aws.ToString(o.S3OriginConfig.OriginAccessIdentity) != "" {
				origin.S3OriginConfig = &S3OriginConfig{
					OriginAccessIdentity: aws.ToString(o.S3OriginConfig.OriginAccessIdentity),
				}
			}

			// Add warning if no access control configured for S3 origin
			if strings.Contains(origin.DomainName, ".s3.") || strings.Contains(origin.DomainName, ".s3-") {
				if origin.OriginAccessControlId == "" && (origin.S3OriginConfig == nil || origin.S3OriginConfig.OriginAccessIdentity == "") {
					origin.OriginAccessConfigWarning = "No OAC or OAI configured - S3 bucket must allow public access or have a bucket policy"
				}
			}

			origins = append(origins, origin)
		}
	}

	return json.MarshalIndent(origins, "", "  ")
}

func (p *CloudFrontProvider) getFunctionCode(ctx context.Context, functionName string) ([]byte, error) {
	resp, err := p.client.GetFunction(ctx, &cloudfront.GetFunctionInput{
		Name: aws.String(functionName),
	})
	if err != nil {
		return nil, err
	}

	return resp.FunctionCode, nil
}

func (p *CloudFrontProvider) getFunctionConfig(ctx context.Context, functionName string) ([]byte, error) {
	resp, err := p.client.DescribeFunction(ctx, &cloudfront.DescribeFunctionInput{
		Name: aws.String(functionName),
	})
	if err != nil {
		return nil, err
	}

	type FuncConfig struct {
		Name    string `json:"Name"`
		Status  string `json:"Status"`
		Runtime string `json:"Runtime"`
		Comment string `json:"Comment,omitempty"`
	}

	config := FuncConfig{
		Name:    aws.ToString(resp.FunctionSummary.Name),
		Status:  aws.ToString(resp.FunctionSummary.Status),
		Runtime: string(resp.FunctionSummary.FunctionConfig.Runtime),
		Comment: aws.ToString(resp.FunctionSummary.FunctionConfig.Comment),
	}

	return json.MarshalIndent(config, "", "  ")
}

func (p *CloudFrontProvider) getFunctionLogs(ctx context.Context, functionName string) ([]byte, error) {
	logGroupName := fmt.Sprintf("/aws/cloudfront/function/%s", functionName)

	startTime := time.Now().Add(-1 * time.Hour).UnixMilli()

	resp, err := p.logsClient.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(logGroupName),
		StartTime:    aws.Int64(startTime),
		Limit:        aws.Int32(500),
	})
	if err != nil {
		// Log group might not exist
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return []byte(fmt.Sprintf("# No CloudWatch log group found: %s\n# CloudFront function logs may not be enabled\n", logGroupName)), nil
		}
		return nil, err
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("# Log group: %s\n\n", logGroupName))

	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		buf.WriteString(fmt.Sprintf("[%s] %s", ts, msg))
	}

	if len(resp.Events) == 0 {
		buf.WriteString("# No events in the last hour\n")
	}

	return []byte(buf.String()), nil
}

func (p *CloudFrontProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *CloudFrontProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "cloudfront", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")

	// distributions or functions
	if len(parts) == 1 {
		if parts[0] == "distributions" || parts[0] == "functions" {
			return &Entry{Name: parts[0], IsDir: true}, nil
		}
	}

	// distributions/<id> or functions/<name>
	if len(parts) == 2 {
		return &Entry{Name: parts[1], IsDir: true}, nil
	}

	// distributions/<id>/info.json, origins.json or functions/<name>/code.js, config.json, logs
	if len(parts) == 3 {
		switch parts[2] {
		case "info.json", "origins.json", "code.js", "config.json":
			return &Entry{Name: parts[2], IsDir: false, Size: 4096}, nil
		case "logs":
			return &Entry{Name: "logs", IsDir: true}, nil
		}
	}

	// functions/<name>/logs/latest.log
	if len(parts) == 4 && parts[2] == "logs" && parts[3] == "latest.log" {
		return &Entry{Name: "latest.log", IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// Streaming support for logs

type streamingCloudFrontLogFile struct {
	client       *cloudwatchlogs.Client
	logGroupName string
	buffer       []byte
	readOffset   int
	nextToken    *string
	fullyLoaded  bool
	ctx          context.Context
}

func (p *CloudFrontProvider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	if !strings.HasSuffix(path, "/logs/latest.log") {
		return nil, nil
	}

	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[0] != "functions" {
		return nil, nil
	}

	functionName := parts[1]
	logGroupName := fmt.Sprintf("/aws/cloudfront/function/%s", functionName)

	// Check if log group exists
	_, err := p.logsClient.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(logGroupName),
		Limit:              aws.Int32(1),
	})
	if err != nil {
		return &streamingCloudFrontLogFile{
			client:      p.logsClient,
			buffer:      []byte(fmt.Sprintf("# No CloudWatch log group found: %s\n", logGroupName)),
			fullyLoaded: true,
			ctx:         ctx,
		}, nil
	}

	return &streamingCloudFrontLogFile{
		client:       p.logsClient,
		logGroupName: logGroupName,
		ctx:          ctx,
	}, nil
}

func (f *streamingCloudFrontLogFile) Read(p []byte) (int, error) {
	for f.readOffset >= len(f.buffer) && !f.fullyLoaded {
		if err := f.fetchNextPage(); err != nil {
			return 0, err
		}
	}

	if f.readOffset >= len(f.buffer) {
		return 0, io.EOF
	}

	n := copy(p, f.buffer[f.readOffset:])
	f.readOffset += n

	if f.readOffset >= len(f.buffer) && f.fullyLoaded {
		return n, io.EOF
	}

	return n, nil
}

func (f *streamingCloudFrontLogFile) fetchNextPage() error {
	if f.logGroupName == "" {
		f.fullyLoaded = true
		return nil
	}

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

func (f *streamingCloudFrontLogFile) Close() error {
	f.buffer = nil
	return nil
}

func (f *streamingCloudFrontLogFile) Size() int64 {
	return -1
}
