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
			{Name: "logs", IsDir: true},
		}, nil
	}

	// Task logs directory
	if len(parts) == 5 && parts[2] == "tasks" && parts[4] == "logs" {
		return []Entry{
			{Name: "latest.log", IsDir: false},
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
			entry := Entry{
				Name:  name,
				IsDir: true,
			}
			entries = append(entries, entry)
			// Pre-cache stat to avoid extra API call
			p.cache.Set("stat:"+name, &entry)
		}
	}

	return entries, nil
}

func (p *ECSProvider) listServices(ctx context.Context, clusterName string) ([]Entry, error) {
	var entries []Entry
	var serviceArns []string

	paginator := ecs.NewListServicesPaginator(p.client, &ecs.ListServicesInput{
		Cluster: aws.String(clusterName),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		serviceArns = append(serviceArns, page.ServiceArns...)
	}

	// Batch describe services to get deployment times (max 10 per call)
	for i := 0; i < len(serviceArns); i += 10 {
		end := i + 10
		if end > len(serviceArns) {
			end = len(serviceArns)
		}
		batch := serviceArns[i:end]

		resp, err := p.client.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  aws.String(clusterName),
			Services: batch,
		})
		if err != nil {
			// Fall back to basic entries without modtime
			for _, arn := range batch {
				parts := strings.Split(arn, "/")
				name := parts[len(parts)-1]
				entries = append(entries, Entry{Name: name, IsDir: true})
			}
			continue
		}

		for _, svc := range resp.Services {
			name := aws.ToString(svc.ServiceName)
			var modTime time.Time
			if len(svc.Deployments) > 0 && svc.Deployments[0].UpdatedAt != nil {
				modTime = *svc.Deployments[0].UpdatedAt
			}
			entry := Entry{
				Name:    name,
				IsDir:   true,
				ModTime: modTime,
			}
			entries = append(entries, entry)
			// Pre-cache stat to avoid extra API call
			p.cache.Set("stat:"+clusterName+"/"+name, &entry)
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

	if len(resp.TaskArns) == 0 {
		return entries, nil
	}

	// Batch describe tasks to get StartedAt times (max 100 per call)
	descResp, err := p.client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(clusterName),
		Tasks:   resp.TaskArns,
	})
	if err != nil {
		// Fall back to basic entries without modtime
		for _, arn := range resp.TaskArns {
			parts := strings.Split(arn, "/")
			taskID := parts[len(parts)-1]
			entries = append(entries, Entry{Name: taskID, IsDir: true})
		}
		return entries, nil
	}

	for _, task := range descResp.Tasks {
		parts := strings.Split(aws.ToString(task.TaskArn), "/")
		taskID := parts[len(parts)-1]
		var modTime time.Time
		if task.StartedAt != nil {
			modTime = *task.StartedAt
		}
		entry := Entry{
			Name:    taskID,
			IsDir:   true,
			ModTime: modTime,
		}
		entries = append(entries, entry)
		// Pre-cache stat to avoid extra API call
		p.cache.Set("stat:"+clusterName+"/"+serviceName+"/tasks/"+taskID, &entry)
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
	cacheKey := fmt.Sprintf("loggroup:%s/%s", clusterName, serviceName)
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(string), nil
	}

	logGroup, err := p.getLogGroupFromTaskDefUncached(ctx, clusterName, serviceName)
	if err == nil {
		p.cache.Set(cacheKey, logGroup)
	}
	return logGroup, err
}

func (p *ECSProvider) getLogGroupFromTaskDefUncached(ctx context.Context, clusterName, serviceName string) (string, error) {
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

// logConfig holds log configuration for a task
type logConfig struct {
	logGroup        string
	logStreamPrefix string
	containerName   string
}

func (p *ECSProvider) getLogConfigFromTask(ctx context.Context, clusterName, taskID string) (*logConfig, error) {
	cacheKey := fmt.Sprintf("logconfig:%s/%s", clusterName, taskID)
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(*logConfig), nil
	}

	cfg, err := p.getLogConfigFromTaskUncached(ctx, clusterName, taskID)
	if err == nil {
		p.cache.Set(cacheKey, cfg)
	}
	return cfg, err
}

func (p *ECSProvider) getLogConfigFromTaskUncached(ctx context.Context, clusterName, taskID string) (*logConfig, error) {
	// Get task to find task definition ARN
	taskResp, err := p.client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(clusterName),
		Tasks:   []string{taskID},
	})
	if err != nil {
		return nil, err
	}
	if len(taskResp.Tasks) == 0 {
		return nil, fmt.Errorf("task not found")
	}

	taskDefArn := aws.ToString(taskResp.Tasks[0].TaskDefinitionArn)
	if taskDefArn == "" {
		return nil, fmt.Errorf("no task definition")
	}

	// Describe task definition to get log configuration
	taskDefResp, err := p.client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefArn),
	})
	if err != nil {
		return nil, err
	}

	// Check container definitions for awslogs configuration
	if taskDefResp.TaskDefinition != nil {
		for _, container := range taskDefResp.TaskDefinition.ContainerDefinitions {
			if container.LogConfiguration != nil && container.LogConfiguration.Options != nil {
				logGroup, hasGroup := container.LogConfiguration.Options["awslogs-group"]
				if hasGroup {
					cfg := &logConfig{
						logGroup:      logGroup,
						containerName: aws.ToString(container.Name),
					}
					if prefix, hasPrefix := container.LogConfiguration.Options["awslogs-stream-prefix"]; hasPrefix {
						cfg.logStreamPrefix = prefix
					}
					return cfg, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no log group in task definition")
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
			data, err := p.Read(ctx, path)
			size := int64(4096)
			if err == nil {
				size = int64(len(data))
			}
			return &Entry{Name: "info.json", IsDir: false, Size: size}, nil
		}
		// Service directory - get modtime from latest deployment
		resp, err := p.client.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  aws.String(parts[0]),
			Services: []string{parts[1]},
		})
		var modTime time.Time
		if err == nil && len(resp.Services) > 0 && len(resp.Services[0].Deployments) > 0 {
			modTime = aws.ToTime(resp.Services[0].Deployments[0].UpdatedAt)
		}
		return &Entry{Name: parts[1], IsDir: true, ModTime: modTime}, nil
	}

	// cluster/service/info.json, tasks, logs
	if len(parts) == 3 {
		switch parts[2] {
		case "info.json":
			data, err := p.Read(ctx, path)
			size := int64(4096)
			if err == nil {
				size = int64(len(data))
			}
			// Get modtime from service deployment
			var modTime time.Time
			resp, err := p.client.DescribeServices(ctx, &ecs.DescribeServicesInput{
				Cluster:  aws.String(parts[0]),
				Services: []string{parts[1]},
			})
			if err == nil && len(resp.Services) > 0 && len(resp.Services[0].Deployments) > 0 {
				modTime = aws.ToTime(resp.Services[0].Deployments[0].UpdatedAt)
			}
			return &Entry{Name: "info.json", IsDir: false, Size: size, ModTime: modTime}, nil
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
			// Task directory - get modtime from task StartedAt
			resp, err := p.client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
				Cluster: aws.String(parts[0]),
				Tasks:   []string{parts[3]},
			})
			var modTime time.Time
			if err == nil && len(resp.Tasks) > 0 {
				modTime = aws.ToTime(resp.Tasks[0].StartedAt)
			}
			return &Entry{Name: parts[3], IsDir: true, ModTime: modTime}, nil
		}
	}

	// cluster/service/tasks/task-id/info.json or logs
	if len(parts) == 5 && parts[2] == "tasks" {
		switch parts[4] {
		case "info.json":
			data, err := p.Read(ctx, path)
			size := int64(4096)
			if err == nil {
				size = int64(len(data))
			}
			// Get modtime from task StartedAt
			var modTime time.Time
			resp, err := p.client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
				Cluster: aws.String(parts[0]),
				Tasks:   []string{parts[3]},
			})
			if err == nil && len(resp.Tasks) > 0 {
				modTime = aws.ToTime(resp.Tasks[0].StartedAt)
			}
			return &Entry{Name: "info.json", IsDir: false, Size: size, ModTime: modTime}, nil
		case "logs":
			return &Entry{Name: "logs", IsDir: true}, nil
		}
	}

	// cluster/service/tasks/task-id/logs/latest.log
	if len(parts) == 6 && parts[2] == "tasks" && parts[4] == "logs" && parts[5] == "latest.log" {
		return &Entry{Name: "latest.log", IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// Streaming support for logs

type streamingECSLogFile struct {
	client          *cloudwatchlogs.Client
	logGroupName    string
	logStreamPrefix string
	buffer          []byte
	readOffset      int
	nextToken       *string
	fullyLoaded     bool
	ctx             context.Context
}

func (p *ECSProvider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	if !strings.HasSuffix(path, "/logs/latest.log") {
		return nil, nil
	}

	parts := strings.Split(path, "/")

	// Task-level logs: cluster/service/tasks/task-id/logs/latest.log
	if len(parts) == 6 && parts[2] == "tasks" {
		return p.openTaskLogStream(ctx, parts[0], parts[3])
	}

	// Service-level logs: cluster/service/logs/latest.log
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

func (p *ECSProvider) openTaskLogStream(ctx context.Context, clusterName, taskID string) (StreamingFile, error) {
	cfg, err := p.getLogConfigFromTask(ctx, clusterName, taskID)
	if err != nil {
		return &streamingECSLogFile{
			client:      p.logsClient,
			buffer:      []byte(fmt.Sprintf("# Could not get log config for task %s: %v\n", taskID, err)),
			fullyLoaded: true,
			ctx:         ctx,
		}, nil
	}

	// Build log stream prefix: prefix/container-name/task-id
	var logStreamPrefix string
	if cfg.logStreamPrefix != "" {
		logStreamPrefix = fmt.Sprintf("%s/%s/%s", cfg.logStreamPrefix, cfg.containerName, taskID)
	}

	return &streamingECSLogFile{
		client:          p.logsClient,
		logGroupName:    cfg.logGroup,
		logStreamPrefix: logStreamPrefix,
		ctx:             ctx,
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
	if f.logStreamPrefix != "" {
		input.LogStreamNamePrefix = aws.String(f.logStreamPrefix)
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
		if f.logStreamPrefix != "" {
			f.buffer = append(f.buffer, []byte(fmt.Sprintf("# Log group: %s\n# Log stream: %s\n\n", f.logGroupName, f.logStreamPrefix))...)
		} else {
			f.buffer = append(f.buffer, []byte(fmt.Sprintf("# Log group: %s\n\n", f.logGroupName))...)
		}
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
