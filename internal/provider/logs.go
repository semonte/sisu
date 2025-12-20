package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/semonte/sisu/internal/cache"
)

// LogsProvider provides access to CloudWatch Logs
type LogsProvider struct {
	ReadOnlyProvider
	client *cloudwatchlogs.Client
	cache  *cache.Cache
}

// NewLogsProvider creates a new CloudWatch Logs provider
func NewLogsProvider(profile, region string) (*LogsProvider, error) {
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

	return &LogsProvider{
		client: cloudwatchlogs.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *LogsProvider) Name() string {
	return "logs"
}

func (p *LogsProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *LogsProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Get all log groups and filter by path prefix
	allGroups, err := p.listAllLogGroups(ctx)
	if err != nil {
		return nil, err
	}

	// Check if path points to a log stream (group + stream name)
	_, streamName := p.parseStreamPath(path, allGroups)
	if streamName != "" {
		// Inside a stream directory
		return []Entry{
			{Name: "events.log", IsDir: false},
			{Name: "info.json", IsDir: false},
		}, nil
	}

	// Check if path points to an actual log group
	for _, g := range allGroups {
		fsName := strings.TrimPrefix(g, "/")
		if fsName == path {
			// Log group directory: show info, latest, and streams
			entries := []Entry{
				{Name: "info.json", IsDir: false},
				{Name: "latest.log", IsDir: false},
			}

			// Add stream directories (limit to 50)
			streams, err := p.listStreams(ctx, g, 50)
			if err == nil {
				for _, stream := range streams {
					// Escape slashes in stream names
					safeName := strings.ReplaceAll(stream, "/", "_")
					entries = append(entries, Entry{
						Name:  safeName,
						IsDir: true,
					})
				}
			}
			return entries, nil
		}
	}

	// Otherwise, list log groups/virtual directories
	var entries []Entry
	seen := make(map[string]bool)

	prefix := path
	if prefix != "" {
		prefix += "/"
	}

	for _, groupName := range allGroups {
		// Remove leading slash from log group name for filesystem
		fsName := strings.TrimPrefix(groupName, "/")

		// Check if this group is under the current path
		if !strings.HasPrefix(fsName, prefix) && path != "" {
			continue
		}

		// Get the relative name
		relName := fsName
		if prefix != "" {
			relName = strings.TrimPrefix(fsName, prefix)
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
			// It's a log group (directory with files)
			entries = append(entries, Entry{
				Name:  relName,
				IsDir: true,
			})
		}
	}

	return entries, nil
}

// parseStreamPath checks if path ends with a stream name and returns group and stream
func (p *LogsProvider) parseStreamPath(path string, allGroups []string) (groupName, streamName string) {
	for _, g := range allGroups {
		fsGroup := strings.TrimPrefix(g, "/")
		if strings.HasPrefix(path, fsGroup+"/") {
			remaining := strings.TrimPrefix(path, fsGroup+"/")
			// The remaining part could be a stream name (with _ instead of /)
			if remaining != "" && remaining != "info.json" && remaining != "latest.log" {
				return g, remaining
			}
		}
	}
	return "", ""
}

func (p *LogsProvider) listStreams(ctx context.Context, groupName string, limit int32) ([]string, error) {
	cacheKey := "streams:" + groupName
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]string), nil
	}

	resp, err := p.client.DescribeLogStreams(ctx, &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName: aws.String(groupName),
		OrderBy:      "LastEventTime",
		Descending:   aws.Bool(true),
		Limit:        aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}

	var streams []string
	for _, stream := range resp.LogStreams {
		streams = append(streams, aws.ToString(stream.LogStreamName))
	}

	p.cache.Set(cacheKey, streams)
	return streams, nil
}

func (p *LogsProvider) listAllLogGroups(ctx context.Context) ([]string, error) {
	cacheKey := "allgroups"
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]string), nil
	}

	var groups []string
	var nextToken *string

	for {
		resp, err := p.client.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}

		for _, group := range resp.LogGroups {
			groups = append(groups, aws.ToString(group.LogGroupName))
		}

		if resp.NextToken == nil {
			break
		}
		nextToken = resp.NextToken
	}

	p.cache.Set(cacheKey, groups)
	return groups, nil
}

func (p *LogsProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *LogsProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	// Get all log groups to parse the path
	allGroups, err := p.listAllLogGroups(ctx)
	if err != nil {
		return nil, err
	}

	// Check if this is a stream file (events.log or info.json inside a stream)
	if strings.HasSuffix(path, "/events.log") {
		streamPath := strings.TrimSuffix(path, "/events.log")
		groupName, streamName := p.parseStreamPath(streamPath, allGroups)
		if groupName != "" && streamName != "" {
			// Convert escaped stream name back to original
			realStreamName := strings.ReplaceAll(streamName, "_", "/")
			return p.getStreamEvents(ctx, groupName, realStreamName)
		}
	}

	// Check for stream info.json
	if strings.HasSuffix(path, "/info.json") {
		infoPath := strings.TrimSuffix(path, "/info.json")
		groupName, streamName := p.parseStreamPath(infoPath, allGroups)
		if groupName != "" && streamName != "" {
			realStreamName := strings.ReplaceAll(streamName, "_", "/")
			return p.getStreamInfo(ctx, groupName, realStreamName)
		}
		// Otherwise it's a log group info.json
		groupName = "/" + infoPath
		return p.getLogGroupInfo(ctx, groupName)
	}

	if strings.HasSuffix(path, "/latest.log") {
		groupName := "/" + strings.TrimSuffix(path, "/latest.log")
		return p.getLatestEvents(ctx, groupName)
	}

	return nil, fmt.Errorf("unknown file: %s", path)
}

func (p *LogsProvider) getLogGroupInfo(ctx context.Context, groupName string) ([]byte, error) {
	resp, err := p.client.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(groupName),
	})
	if err != nil {
		return nil, err
	}

	for _, group := range resp.LogGroups {
		if aws.ToString(group.LogGroupName) == groupName {
			return json.MarshalIndent(group, "", "  ")
		}
	}

	return nil, fmt.Errorf("log group not found: %s", groupName)
}

func (p *LogsProvider) getStreamInfo(ctx context.Context, groupName, streamName string) ([]byte, error) {
	resp, err := p.client.DescribeLogStreams(ctx, &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(groupName),
		LogStreamNamePrefix: aws.String(streamName),
		Limit:               aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}

	for _, stream := range resp.LogStreams {
		if aws.ToString(stream.LogStreamName) == streamName {
			info := map[string]interface{}{
				"LogStreamName": aws.ToString(stream.LogStreamName),
				"Arn":           aws.ToString(stream.Arn),
			}
			if stream.LastEventTimestamp != nil {
				info["LastEventTime"] = time.UnixMilli(*stream.LastEventTimestamp).Format(time.RFC3339)
			}
			if stream.FirstEventTimestamp != nil {
				info["FirstEventTime"] = time.UnixMilli(*stream.FirstEventTimestamp).Format(time.RFC3339)
			}
			return json.MarshalIndent(info, "", "  ")
		}
	}

	return nil, fmt.Errorf("stream not found: %s", streamName)
}

func (p *LogsProvider) getStreamEvents(ctx context.Context, groupName, streamName string) ([]byte, error) {
	// Use FilterLogEvents which works better for fetching recent events from a stream
	resp, err := p.client.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:   aws.String(groupName),
		LogStreamNames: []string{streamName},
		Limit:          aws.Int32(500),
	})
	if err != nil {
		return nil, fmt.Errorf("FilterLogEvents failed: %w\nGroup: %s\nStream: %s", err, groupName, streamName)
	}

	if len(resp.Events) == 0 {
		return []byte(fmt.Sprintf("# No events found\n# Group: %s\n# Stream: %s\n", groupName, streamName)), nil
	}

	// FilterLogEvents returns events in chronological order
	var buf strings.Builder
	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		buf.WriteString(fmt.Sprintf("[%s] %s", ts, msg))
	}

	return []byte(buf.String()), nil
}

func (p *LogsProvider) getLatestEvents(ctx context.Context, groupName string) ([]byte, error) {
	// Get events from the last hour
	startTime := time.Now().Add(-1 * time.Hour).UnixMilli()

	resp, err := p.client.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(groupName),
		StartTime:    aws.Int64(startTime),
		Limit:        aws.Int32(500), // Limit to recent events
	})
	if err != nil {
		return nil, err
	}

	var buf strings.Builder
	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		// Ensure each line ends with newline
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

func (p *LogsProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *LogsProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "logs", IsDir: true}, nil
	}

	// Check if it's a file
	if strings.HasSuffix(path, "/info.json") ||
		strings.HasSuffix(path, "/latest.log") {
		parts := strings.Split(path, "/")
		return &Entry{Name: parts[len(parts)-1], IsDir: false, Size: 4096}, nil
	}

	// events.log is streaming - report 1MB size
	// Tools will seek based on this, but we fully load on any seek
	if strings.HasSuffix(path, "/events.log") {
		parts := strings.Split(path, "/")
		return &Entry{Name: parts[len(parts)-1], IsDir: false, Size: 1024 * 1024}, nil
	}

	// Check if it's an exact log group match
	allGroups, err := p.listAllLogGroups(ctx)
	if err != nil {
		return nil, err
	}

	for _, groupName := range allGroups {
		fsName := strings.TrimPrefix(groupName, "/")
		if fsName == path {
			// It's an actual log group - show as directory
			return &Entry{Name: path, IsDir: true}, nil
		}
	}

	// Check if it's a stream directory
	groupName, streamName := p.parseStreamPath(path, allGroups)
	if groupName != "" && streamName != "" {
		// Verify stream exists
		realStreamName := strings.ReplaceAll(streamName, "_", "/")
		streams, err := p.listStreams(ctx, groupName, 50)
		if err == nil {
			for _, s := range streams {
				if s == realStreamName {
					return &Entry{Name: streamName, IsDir: true}, nil
				}
			}
		}
	}

	// Check if it's a prefix (virtual directory for log groups)
	prefix := path + "/"
	for _, groupName := range allGroups {
		fsName := strings.TrimPrefix(groupName, "/")
		if strings.HasPrefix(fsName, prefix) {
			return &Entry{Name: path, IsDir: true}, nil
		}
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// streamingLogFile implements StreamingFile for CloudWatch Logs
type streamingLogFile struct {
	client      *cloudwatchlogs.Client
	groupName   string
	streamName  string
	buffer      []byte
	readOffset  int
	nextToken   *string
	fullyLoaded bool
	ctx         context.Context
}

// OpenStream opens a streaming file for events.log files
func (p *LogsProvider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	// Only stream events.log files
	if !strings.HasSuffix(path, "/events.log") {
		return nil, nil
	}

	allGroups, err := p.listAllLogGroups(ctx)
	if err != nil {
		return nil, err
	}

	streamPath := strings.TrimSuffix(path, "/events.log")
	groupName, streamName := p.parseStreamPath(streamPath, allGroups)
	if groupName == "" || streamName == "" {
		return nil, fmt.Errorf("invalid stream path: %s", path)
	}

	// Convert escaped stream name back to original
	realStreamName := strings.ReplaceAll(streamName, "_", "/")

	return &streamingLogFile{
		client:     p.client,
		groupName:  groupName,
		streamName: realStreamName,
		ctx:        ctx,
	}, nil
}

func (f *streamingLogFile) Read(p []byte) (int, error) {
	if Debug {
		log.Printf("[logs] Read: readOffset=%d, bufLen=%d, fullyLoaded=%v, pLen=%d",
			f.readOffset, len(f.buffer), f.fullyLoaded, len(p))
	}

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

func (f *streamingLogFile) fetchNextPage() error {
	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:   aws.String(f.groupName),
		LogStreamNames: []string{f.streamName},
		Limit:          aws.Int32(100), // Fetch 100 events per page
	}
	if f.nextToken != nil {
		input.NextToken = f.nextToken
		if Debug {
			log.Printf("[logs] fetchNextPage: using nextToken (len=%d)", len(*f.nextToken))
		}
	} else {
		// Only set StartTime on first request, not when paginating
		input.StartTime = aws.Int64(0)
		if Debug {
			log.Printf("[logs] fetchNextPage: first request (no token)")
		}
	}

	resp, err := f.client.FilterLogEvents(f.ctx, input)
	if err != nil {
		return err
	}

	if Debug {
		log.Printf("[logs] fetchNextPage: got %d events, nextToken=%v", len(resp.Events), resp.NextToken != nil)
	}

	// Append events to buffer
	for _, event := range resp.Events {
		ts := time.UnixMilli(aws.ToInt64(event.Timestamp)).Format("2006-01-02 15:04:05")
		msg := aws.ToString(event.Message)
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		line := fmt.Sprintf("[%s] %s", ts, msg)
		f.buffer = append(f.buffer, []byte(line)...)
	}

	f.nextToken = resp.NextToken
	if f.nextToken == nil {
		f.fullyLoaded = true
		if Debug {
			log.Printf("[logs] fetchNextPage: fully loaded, total buffer size=%d", len(f.buffer))
		}
	}

	return nil
}

func (f *streamingLogFile) Close() error {
	f.buffer = nil
	return nil
}

func (f *streamingLogFile) Size() int64 {
	return -1 // Unknown size for streaming
}
