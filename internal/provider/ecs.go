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
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/semonte/sisu/internal/cache"
)

// ECSProvider provides access to AWS ECS clusters and services
type ECSProvider struct {
	ReadOnlyProvider
	client     *ecs.Client
	logsClient *cloudwatchlogs.Client
	cache      *cache.Cache
}

// NewECSProvider creates a new ECS provider
func NewECSProvider(profile, region string) (*ECSProvider, error) {
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

	return &ECSProvider{
		client:     ecs.NewFromConfig(cfg),
		logsClient: cloudwatchlogs.NewFromConfig(cfg),
		cache:      cache.New(5 * time.Minute),
	}, nil
}

func (p *ECSProvider) Name() string {
	return "ecs"
}

func (p *ECSProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *ECSProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list all clusters
	if path == "" {
		return p.listClusters(ctx)
	}

	parts := strings.Split(path, "/")
	clusterName := parts[0]

	// Cluster level: show services
	if len(parts) == 1 {
		entries := []Entry{
			{Name: "info.json", IsDir: false},
		}
		services, err := p.listServices(ctx, clusterName)
		if err == nil {
			entries = append(entries, services...)
		}
		return entries, nil
	}

	// Service level
	if len(parts) == 2 {
		serviceName := parts[1]
		if serviceName == "info.json" {
			return nil, fmt.Errorf("not a directory")
		}
		return []Entry{
			{Name: "info.json", IsDir: false},
			{Name: "tasks", IsDir: true},
			{Name: "logs", IsDir: true},
		}, nil
	}

	// Service subdirectories
	if len(parts) == 3 {
		serviceName := parts[1]
		subDir := parts[2]

		if subDir == "tasks" {
			return p.listTasks(ctx, clusterName, serviceName)
		}
		if subDir == "logs" {
			return []Entry{
				{Name: "latest.log", IsDir: false},
			}, nil
		}
	}

	// Task level
	if len(parts) == 4 && parts[2] == "tasks" {
		return []Entry{
			{Name: "info.json", IsDir: false},
		}, nil
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *ECSProvider) listClusters(ctx context.Context) ([]Entry, error) {
	var entries []Entry

	paginator := ecs.NewListClustersPaginator(p.client, &ecs.ListClustersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, arn := range page.ClusterArns {
			// Extract cluster name from ARN
			parts := strings.Split(arn, "/")
			name := parts[len(parts)-1]
			entries = append(entries, Entry{
				Name:  name,
				IsDir: true,
			})
		}
	}

	return entries, nil
}

func (p *ECSProvider) listServices(ctx context.Context, clusterName string) ([]Entry, error) {
	var entries []Entry

	paginator := ecs.NewListServicesPaginator(p.client, &ecs.ListServicesInput{
		Cluster: aws.String(clusterName),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, arn := range page.ServiceArns {
			parts := strings.Split(arn, "/")
			name := parts[len(parts)-1]
			entries = append(entries, Entry{
				Name:  name,
				IsDir: true,
			})
		}
	}

	return entries, nil
}

func (p *ECSProvider) listTasks(ctx context.Context, clusterName, serviceName string) ([]Entry, error) {
	var entries []Entry

	resp, err := p.client.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:     aws.String(clusterName),
		ServiceName: aws.String(serviceName),
	})
	if err != nil {
		return nil, err
	}

	for _, arn := range resp.TaskArns {
		parts := strings.Split(arn, "/")
		taskID := parts[len(parts)-1]
		entries = append(entries, Entry{
			Name:  taskID,
			IsDir: true,
		})
	}

	return entries, nil
}

func (p *ECSProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *ECSProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	parts := strings.Split(path, "/")

	// cluster/info.json
	if len(parts) == 2 && parts[1] == "info.json" {
		return p.getClusterInfo(ctx, parts[0])
	}

	// cluster/service/info.json
	if len(parts) == 3 && parts[2] == "info.json" {
		return p.getServiceInfo(ctx, parts[0], parts[1])
	}

	// cluster/service/logs/latest.log - handled by OpenStream (streaming)

	// cluster/service/tasks/task-id/info.json
	if len(parts) == 5 && parts[2] == "tasks" && parts[4] == "info.json" {
		return p.getTaskInfo(ctx, parts[0], parts[3])
	}

	return nil, fmt.Errorf("unknown file: %s", path)
}

func (p *ECSProvider) getClusterInfo(ctx context.Context, clusterName string) ([]byte, error) {
	resp, err := p.client.DescribeClusters(ctx, &ecs.DescribeClustersInput{
		Clusters: []string{clusterName},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Clusters) == 0 {
		return nil, fmt.Errorf("cluster not found: %s", clusterName)
	}

	return json.MarshalIndent(resp.Clusters[0], "", "  ")
}

func (p *ECSProvider) getServiceInfo(ctx context.Context, clusterName, serviceName string) ([]byte, error) {
	resp, err := p.client.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Services) == 0 {
		return nil, fmt.Errorf("service not found: %s", serviceName)
	}

	return json.MarshalIndent(resp.Services[0], "", "  ")
}

func (p *ECSProvider) getTaskInfo(ctx context.Context, clusterName, taskID string) ([]byte, error) {
	resp, err := p.client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(clusterName),
		Tasks:   []string{taskID},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Tasks) == 0 {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	return json.MarshalIndent(resp.Tasks[0], "", "  ")
}

func (p *ECSProvider) getLogGroupFromTaskDef(ctx context.Context, clusterName, serviceName string) (string, error) {
	// Get service to find task definition ARN
	svcResp, err := p.client.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil {
		return "", err
	}
	if len(svcResp.Services) == 0 {
		return "", fmt.Errorf("service not found")
	}

	taskDefArn := aws.ToString(svcResp.Services[0].TaskDefinition)
	if taskDefArn == "" {
		return "", fmt.Errorf("no task definition")
	}

	// Describe task definition to get log configuration
	taskDefResp, err := p.client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefArn),
	})
	if err != nil {
		return "", err
	}

	// Check container definitions for awslogs configuration
	if taskDefResp.TaskDefinition != nil {
		for _, container := range taskDefResp.TaskDefinition.ContainerDefinitions {
			if container.LogConfiguration != nil && container.LogConfiguration.Options != nil {
				if logGroup, ok := container.LogConfiguration.Options["awslogs-group"]; ok {
					return logGroup, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no log group in task definition")
}


func (p *ECSProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *ECSProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "ecs", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")

	// Cluster
	if len(parts) == 1 {
		return &Entry{Name: parts[0], IsDir: true}, nil
	}

	// cluster/info.json or cluster/service
	if len(parts) == 2 {
		if parts[1] == "info.json" {
			return &Entry{Name: "info.json", IsDir: false, Size: 4096}, nil
		}
		return &Entry{Name: parts[1], IsDir: true}, nil
	}

	// cluster/service/info.json, tasks, logs
	if len(parts) == 3 {
		switch parts[2] {
		case "info.json":
			return &Entry{Name: "info.json", IsDir: false, Size: 4096}, nil
		case "tasks", "logs":
			return &Entry{Name: parts[2], IsDir: true}, nil
		}
	}

	// cluster/service/logs/latest.log or cluster/service/tasks/task-id
	if len(parts) == 4 {
		if parts[2] == "logs" && parts[3] == "latest.log" {
			return &Entry{Name: "latest.log", IsDir: false, Size: 4096}, nil
		}
		if parts[2] == "tasks" {
			return &Entry{Name: parts[3], IsDir: true}, nil
		}
	}

	// cluster/service/tasks/task-id/info.json
	if len(parts) == 5 && parts[2] == "tasks" && parts[4] == "info.json" {
		return &Entry{Name: "info.json", IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// Streaming support for logs

type streamingECSLogFile struct {
	client       *cloudwatchlogs.Client
	logGroupName string
	buffer       []byte
	readOffset   int
	nextToken    *string
	fullyLoaded  bool
	ctx          context.Context
}

func (p *ECSProvider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	if !strings.HasSuffix(path, "/logs/latest.log") {
		return nil, nil
	}

	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return nil, nil
	}

	clusterName := parts[0]
	serviceName := parts[1]

	// Try to get log group from task definition first
	logGroupName, err := p.getLogGroupFromTaskDef(ctx, clusterName, serviceName)
	if err == nil && logGroupName != "" {
		return &streamingECSLogFile{
			client:       p.logsClient,
			logGroupName: logGroupName,
			ctx:          ctx,
		}, nil
	}

	// Fallback: try common log group patterns
	logGroupPatterns := []string{
		fmt.Sprintf("/ecs/%s", serviceName),
		fmt.Sprintf("/ecs/%s/%s", clusterName, serviceName),
		fmt.Sprintf("ecs/%s", serviceName),
	}

	for _, pattern := range logGroupPatterns {
		resp, err := p.logsClient.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: aws.String(pattern),
			Limit:              aws.Int32(1),
		})
		if err == nil && len(resp.LogGroups) > 0 {
			return &streamingECSLogFile{
				client:       p.logsClient,
				logGroupName: aws.ToString(resp.LogGroups[0].LogGroupName),
				ctx:          ctx,
			}, nil
		}
	}

	// No log group found
	return &streamingECSLogFile{
		client:      p.logsClient,
		buffer:      []byte(fmt.Sprintf("# No CloudWatch log groups found for service %s/%s\n", clusterName, serviceName)),
		fullyLoaded: true,
		ctx:         ctx,
	}, nil
}

func (f *streamingECSLogFile) Read(p []byte) (int, error) {
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

func (f *streamingECSLogFile) fetchNextPage() error {
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

func (f *streamingECSLogFile) Close() error {
	f.buffer = nil
	return nil
}

func (f *streamingECSLogFile) Size() int64 {
	return -1
}
