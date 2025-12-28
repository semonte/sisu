package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/semonte/sisu/internal/cache"
	"github.com/semonte/sisu/internal/tunnel"
)

// EC2Provider provides access to AWS EC2 instances
type EC2Provider struct {
	ReadOnlyProvider
	client     *ec2.Client
	logsClient *cloudwatchlogs.Client
	cache      *cache.Cache
	tunnelMgr  *tunnel.Manager
	profile    string
	region     string
}

// NewEC2Provider creates a new EC2 provider
func NewEC2Provider(profile, region string) (*EC2Provider, error) {
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

	return &EC2Provider{
		client:     ec2.NewFromConfig(cfg),
		logsClient: cloudwatchlogs.NewFromConfig(cfg),
		cache:      cache.New(5 * time.Minute),
		tunnelMgr:  tunnel.NewManager(profile, region),
		profile:    profile,
		region:     region,
	}, nil
}

func (p *EC2Provider) Name() string {
	return "ec2"
}

func (p *EC2Provider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *EC2Provider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list all instances
	if path == "" {
		return p.listInstances(ctx)
	}

	// Instance directory: show files
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return []Entry{
			{Name: "info.json", IsDir: false},
			{Name: "security-groups.json", IsDir: false},
			{Name: "tags.json", IsDir: false},
			{Name: "console.log", IsDir: false},
			{Name: "connect", IsDir: false, Executable: true},
			{Name: "fs", IsDir: true},
			{Name: "logs", IsDir: true},
		}, nil
	}

	// Handle fs/ paths - remote filesystem
	instanceID := parts[0]
	subPath := parts[1]
	if subPath == "fs" {
		return p.listRemoteDir(ctx, instanceID, "/")
	}
	if strings.HasPrefix(subPath, "fs/") {
		remotePath := "/" + strings.TrimPrefix(subPath, "fs/")
		return p.listRemoteDir(ctx, instanceID, remotePath)
	}

	// Handle logs/ paths - CloudWatch logs
	if subPath == "logs" {
		return p.listInstanceLogs(ctx, instanceID)
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *EC2Provider) listInstances(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	var nextToken *string

	for {
		resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			NextToken: nextToken,
			Filters: []ec2types.Filter{
				{
					Name:   aws.String("instance-state-name"),
					Values: []string{"running"},
				},
			},
		})
		if err != nil {
			return nil, err
		}

		for _, reservation := range resp.Reservations {
			for _, instance := range reservation.Instances {
				entries = append(entries, Entry{
					Name:  aws.ToString(instance.InstanceId),
					IsDir: true,
				})
			}
		}

		if resp.NextToken == nil {
			break
		}
		nextToken = resp.NextToken
	}

	return entries, nil
}

func (p *EC2Provider) listInstanceLogs(ctx context.Context, instanceID string) ([]Entry, error) {
	// Just show latest.log - the read will try to find matching log groups
	return []Entry{
		{Name: "latest.log", IsDir: false},
	}, nil
}

func (p *EC2Provider) getInstanceLogs(ctx context.Context, instanceID string) ([]byte, error) {
	// Search for log groups that might belong to this instance
	// Common patterns: containing instance ID, /ec2/, etc.
	var logGroups []string

	paginator := cloudwatchlogs.NewDescribeLogGroupsPaginator(p.logsClient, &cloudwatchlogs.DescribeLogGroupsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			break
		}
		for _, lg := range page.LogGroups {
			name := aws.ToString(lg.LogGroupName)
			// Check if log group name contains instance ID
			if strings.Contains(name, instanceID) {
				logGroups = append(logGroups, name)
			}
		}
		// Limit search to first 100 groups to avoid long waits
		if len(logGroups) > 0 {
			break
		}
	}

	if len(logGroups) == 0 {
		return []byte(fmt.Sprintf("# No CloudWatch log groups found for instance %s\n# Tip: Use fs/var/log/ to access system logs via SSM\n", instanceID)), nil
	}

	// Get latest events from the first matching log group
	logGroupName := logGroups[0]
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

func (p *EC2Provider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *EC2Provider) readUncached(ctx context.Context, path string) ([]byte, error) {
	if Debug {
		fmt.Printf("DEBUG EC2 Read: path=%q\n", path)
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	instanceID := parts[0]
	subPath := parts[1]
	if Debug {
		fmt.Printf("DEBUG EC2 Read: instanceID=%q subPath=%q\n", instanceID, subPath)
	}

	// Handle fs/ paths - remote filesystem
	if strings.HasPrefix(subPath, "fs/") {
		remotePath := "/" + strings.TrimPrefix(subPath, "fs/")
		if Debug {
			fmt.Printf("DEBUG EC2 Read: remotePath=%q\n", remotePath)
		}
		data, err := p.readRemoteFile(ctx, instanceID, remotePath)
		if Debug {
			fmt.Printf("DEBUG EC2 Read: got %d bytes, err=%v\n", len(data), err)
		}
		return data, err
	}

	// Handle logs/ paths - CloudWatch logs
	if subPath == "logs/latest.log" {
		return p.getInstanceLogs(ctx, instanceID)
	}

	switch subPath {
	case "info.json":
		return p.getInstanceInfo(ctx, instanceID)
	case "security-groups.json":
		return p.getSecurityGroups(ctx, instanceID)
	case "tags.json":
		return p.getTags(ctx, instanceID)
	case "console.log":
		return p.getConsoleOutput(ctx, instanceID)
	case "connect":
		return p.getConnectScript(ctx, instanceID)
	}

	return nil, fmt.Errorf("unknown file: %s", subPath)
}

func (p *EC2Provider) getInstanceInfo(ctx context.Context, instanceID string) ([]byte, error) {
	resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance not found: %s", instanceID)
	}

	return json.MarshalIndent(resp.Reservations[0].Instances[0], "", "  ")
}

func (p *EC2Provider) getSecurityGroups(ctx context.Context, instanceID string) ([]byte, error) {
	resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance not found: %s", instanceID)
	}

	instance := resp.Reservations[0].Instances[0]

	// Get security group IDs
	var sgIDs []string
	for _, sg := range instance.SecurityGroups {
		sgIDs = append(sgIDs, aws.ToString(sg.GroupId))
	}

	if len(sgIDs) == 0 {
		return []byte("[]"), nil
	}

	// Get full security group details
	sgResp, err := p.client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: sgIDs,
	})
	if err != nil {
		return nil, err
	}

	// Format the output
	type Rule struct {
		Protocol string `json:"Protocol"`
		Port     string `json:"Port"`
		Source   string `json:"Source,omitempty"`
		Dest     string `json:"Destination,omitempty"`
	}
	type SGInfo struct {
		GroupID      string `json:"GroupId"`
		GroupName    string `json:"GroupName"`
		Description  string `json:"Description"`
		InboundRules []Rule `json:"InboundRules"`
		OutboundRules []Rule `json:"OutboundRules"`
	}

	var result []SGInfo
	for _, sg := range sgResp.SecurityGroups {
		info := SGInfo{
			GroupID:     aws.ToString(sg.GroupId),
			GroupName:   aws.ToString(sg.GroupName),
			Description: aws.ToString(sg.Description),
		}

		// Parse inbound rules
		for _, perm := range sg.IpPermissions {
			protocol := aws.ToString(perm.IpProtocol)
			if protocol == "-1" {
				protocol = "all"
			}

			port := "all"
			if perm.FromPort != nil {
				if perm.FromPort == perm.ToPort {
					port = fmt.Sprintf("%d", *perm.FromPort)
				} else {
					port = fmt.Sprintf("%d-%d", *perm.FromPort, *perm.ToPort)
				}
			}

			for _, ipRange := range perm.IpRanges {
				info.InboundRules = append(info.InboundRules, Rule{
					Protocol: protocol,
					Port:     port,
					Source:   aws.ToString(ipRange.CidrIp),
				})
			}
			for _, sg := range perm.UserIdGroupPairs {
				info.InboundRules = append(info.InboundRules, Rule{
					Protocol: protocol,
					Port:     port,
					Source:   aws.ToString(sg.GroupId),
				})
			}
		}

		// Parse outbound rules
		for _, perm := range sg.IpPermissionsEgress {
			protocol := aws.ToString(perm.IpProtocol)
			if protocol == "-1" {
				protocol = "all"
			}

			port := "all"
			if perm.FromPort != nil {
				if perm.FromPort == perm.ToPort {
					port = fmt.Sprintf("%d", *perm.FromPort)
				} else {
					port = fmt.Sprintf("%d-%d", *perm.FromPort, *perm.ToPort)
				}
			}

			for _, ipRange := range perm.IpRanges {
				info.OutboundRules = append(info.OutboundRules, Rule{
					Protocol: protocol,
					Port:     port,
					Dest:     aws.ToString(ipRange.CidrIp),
				})
			}
			for _, sg := range perm.UserIdGroupPairs {
				info.OutboundRules = append(info.OutboundRules, Rule{
					Protocol: protocol,
					Port:     port,
					Dest:     aws.ToString(sg.GroupId),
				})
			}
		}

		result = append(result, info)
	}

	return json.MarshalIndent(result, "", "  ")
}

func (p *EC2Provider) getTags(ctx context.Context, instanceID string) ([]byte, error) {
	resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("instance not found: %s", instanceID)
	}

	instance := resp.Reservations[0].Instances[0]

	// Convert tags to a simple map for easier grepping
	tags := make(map[string]string)
	for _, tag := range instance.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	return json.MarshalIndent(tags, "", "  ")
}

func (p *EC2Provider) getConsoleOutput(ctx context.Context, instanceID string) ([]byte, error) {
	resp, err := p.client.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		return nil, err
	}
	if resp.Output == nil {
		return []byte("# No console output available\n"), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(*resp.Output)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func (p *EC2Provider) getConnectScript(ctx context.Context, instanceID string) ([]byte, error) {
	cmd := fmt.Sprintf("aws ssm start-session --target %s", instanceID)
	if p.profile != "" {
		cmd += fmt.Sprintf(" --profile %s", p.profile)
	}
	if p.region != "" {
		cmd += fmt.Sprintf(" --region %s", p.region)
	}
	script := fmt.Sprintf("#!/bin/bash\n%s\n", cmd)
	return []byte(script), nil
}

// listRemoteDir lists a directory on the remote instance via SSM tunnel
func (p *EC2Provider) listRemoteDir(ctx context.Context, instanceID, remotePath string) ([]Entry, error) {
	tun, err := p.tunnelMgr.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to establish SSM tunnel: %w (is session-manager-plugin installed?)", err)
	}

	output, err := tun.ListDir(remotePath)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		// Skip empty lines and total line
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}

		// Parse ls -la output: drwxr-xr-x 2 user group 4096 Jan 1 12:00 filename
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}

		perms := fields[0]
		sizeStr := fields[4]
		// Date is in fields[5], fields[6], fields[7]: "Jan 1 12:00" or "Jan 1 2024"
		dateStr := fields[5] + " " + fields[6] + " " + fields[7]
		name := strings.Join(fields[8:], " ") // filename might have spaces

		// Handle symlinks: "name -> target" - we only want the name
		if idx := strings.Index(name, " -> "); idx != -1 {
			name = name[:idx]
		}

		// Skip . and ..
		if name == "." || name == ".." {
			continue
		}

		size, _ := strconv.ParseInt(sizeStr, 10, 64)
		isDir := strings.HasPrefix(perms, "d") || strings.HasPrefix(perms, "l") // symlinks to dirs act as dirs
		isExec := !isDir && len(perms) > 9 && (perms[3] == 'x' || perms[6] == 'x' || perms[9] == 'x')

		// Parse modification time: "Jan 1 12:00" (recent) or "Jan 1 2024" (older)
		var modTime time.Time
		if t, err := time.Parse("Jan _2 15:04", dateStr); err == nil {
			// Recent file - add current year
			modTime = t.AddDate(time.Now().Year(), 0, 0)
		} else if t, err := time.Parse("Jan _2 2006", dateStr); err == nil {
			// Older file with year
			modTime = t
		}

		entry := Entry{
			Name:       name,
			IsDir:      isDir,
			Size:       size,
			Executable: isExec,
			ModTime:    modTime,
		}
		entries = append(entries, entry)

		// Pre-cache stat for this entry so Stat() doesn't need another call
		childPath := instanceID + "/fs" + remotePath
		if !strings.HasSuffix(childPath, "/") {
			childPath += "/"
		}
		childPath += name
		p.cache.Set("stat:"+childPath, &entry)
	}

	return entries, nil
}

// readRemoteFile reads a file from the remote instance via SSM tunnel
func (p *EC2Provider) readRemoteFile(ctx context.Context, instanceID, remotePath string) ([]byte, error) {
	tun, err := p.tunnelMgr.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to establish SSM tunnel: %w (is session-manager-plugin installed?)", err)
	}

	return tun.ReadFile(remotePath)
}

// writeRemoteFile writes a file to the remote instance via SSM tunnel
func (p *EC2Provider) writeRemoteFile(ctx context.Context, instanceID, remotePath string, data []byte) error {
	tun, err := p.tunnelMgr.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to establish SSM tunnel: %w (is session-manager-plugin installed?)", err)
	}

	return tun.WriteFile(remotePath, data)
}

// Write writes data to a path
func (p *EC2Provider) Write(ctx context.Context, path string, data []byte) error {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid path: %s", path)
	}

	instanceID := parts[0]
	subPath := parts[1]

	// Only fs/ paths are writable
	if !strings.HasPrefix(subPath, "fs/") {
		return fmt.Errorf("cannot write to %s: read-only", subPath)
	}

	remotePath := "/" + strings.TrimPrefix(subPath, "fs/")
	err := p.writeRemoteFile(ctx, instanceID, remotePath, data)
	if err != nil {
		return err
	}

	// Invalidate cache for this path and parent directory
	p.cache.Delete("read:" + path)
	p.cache.Delete("stat:" + path)
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		p.cache.Delete("readdir:" + path[:idx])
	}

	return nil
}

// statRemotePath gets file info from the remote instance via SSM tunnel
func (p *EC2Provider) statRemotePath(ctx context.Context, instanceID, remotePath string) (*Entry, error) {
	tun, err := p.tunnelMgr.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to establish SSM tunnel: %w (is session-manager-plugin installed?)", err)
	}

	output, err := tun.Stat(remotePath)
	if err != nil {
		return nil, err
	}

	// Parse: "regular file 1234 644 1703123456 /path/to/file" or "directory 4096 755 1703123456 /path/to/dir"
	// Note: %F can be multi-word like "regular file", so parse from the end
	fields := strings.Fields(output)
	if len(fields) < 5 {
		return nil, fmt.Errorf("unexpected stat output: %s", output)
	}

	// Parse from the end: path, mtime, perms, size, then type is everything before
	name := fields[len(fields)-1]
	mtime, _ := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	perms, _ := strconv.ParseInt(fields[len(fields)-3], 8, 32)
	size, _ := strconv.ParseInt(fields[len(fields)-4], 10, 64)
	fileType := strings.Join(fields[:len(fields)-4], " ")

	isDir := fileType == "directory"

	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		name = "/"
	}

	isExec := !isDir && (perms&0111) != 0

	return &Entry{
		Name:       name,
		IsDir:      isDir,
		Size:       size,
		Executable: isExec,
		ModTime:    time.Unix(mtime, 0),
	}, nil
}

func (p *EC2Provider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *EC2Provider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "ec2", IsDir: true}, nil
	}

	parts := strings.SplitN(path, "/", 2)

	// Instance directory
	if len(parts) == 1 {
		resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{parts[0]},
		})
		if err != nil || len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
			return nil, fmt.Errorf("instance not found: %s", parts[0])
		}
		var modTime time.Time
		if resp.Reservations[0].Instances[0].LaunchTime != nil {
			modTime = *resp.Reservations[0].Instances[0].LaunchTime
		}
		return &Entry{Name: parts[0], IsDir: true, ModTime: modTime}, nil
	}

	instanceID := parts[0]
	subPath := parts[1]

	// Handle fs directory and fs/ paths
	if subPath == "fs" {
		return &Entry{Name: "fs", IsDir: true}, nil
	}
	if strings.HasPrefix(subPath, "fs/") {
		remotePath := "/" + strings.TrimPrefix(subPath, "fs/")
		return p.statRemotePath(ctx, instanceID, remotePath)
	}

	// Handle logs directory and logs/ paths
	if subPath == "logs" {
		return &Entry{Name: "logs", IsDir: true}, nil
	}
	if subPath == "logs/latest.log" {
		return &Entry{Name: "latest.log", IsDir: false, Size: 4096}, nil
	}

	// Regular files - get real size and modtime from instance
	switch subPath {
	case "info.json", "security-groups.json", "tags.json", "console.log":
		data, err := p.Read(ctx, path)
		size := int64(4096)
		if err == nil {
			size = int64(len(data))
		}
		// Get modtime from instance launch time
		var modTime time.Time
		resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err == nil && len(resp.Reservations) > 0 && len(resp.Reservations[0].Instances) > 0 {
			if resp.Reservations[0].Instances[0].LaunchTime != nil {
				modTime = *resp.Reservations[0].Instances[0].LaunchTime
			}
		}
		return &Entry{Name: subPath, IsDir: false, Size: size, ModTime: modTime}, nil
	case "connect":
		return &Entry{Name: subPath, IsDir: false, Size: 4096, Executable: true}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}

// Delete removes a file from the remote filesystem
func (p *EC2Provider) Delete(ctx context.Context, path string) error {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid path: %s", path)
	}

	instanceID := parts[0]
	subPath := parts[1]

	// Only fs/ paths are deletable
	if !strings.HasPrefix(subPath, "fs/") {
		return fmt.Errorf("cannot delete %s: read-only", subPath)
	}

	remotePath := "/" + strings.TrimPrefix(subPath, "fs/")

	tun, err := p.tunnelMgr.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to establish SSM tunnel: %w", err)
	}

	err = tun.Delete(remotePath)
	if err != nil {
		return err
	}

	// Invalidate cache
	p.cache.Delete("read:" + path)
	p.cache.Delete("stat:" + path)
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		p.cache.Delete("readdir:" + path[:idx])
	}

	return nil
}

// streamingEC2LogFile implements StreamingFile for EC2 logs
type streamingEC2LogFile struct {
	client       *cloudwatchlogs.Client
	logGroupName string
	buffer       []byte
	readOffset   int
	nextToken    *string
	fullyLoaded  bool
	ctx          context.Context
}

// OpenStream opens a streaming file for logs/latest.log
func (p *EC2Provider) OpenStream(ctx context.Context, path string) (StreamingFile, error) {
	// Only stream logs/latest.log files
	if !strings.HasSuffix(path, "/logs/latest.log") {
		return nil, nil
	}

	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return nil, nil
	}

	instanceID := parts[0]

	// Find a log group for this instance
	logGroupName, err := p.findInstanceLogGroup(ctx, instanceID)
	if err != nil || logGroupName == "" {
		// No log group found - return a file with a message
		return &streamingEC2LogFile{
			client:      p.logsClient,
			buffer:      []byte(fmt.Sprintf("# No CloudWatch log groups found for instance %s\n# Tip: Use fs/var/log/ to access system logs via SSM\n", instanceID)),
			fullyLoaded: true,
			ctx:         ctx,
		}, nil
	}

	return &streamingEC2LogFile{
		client:       p.logsClient,
		logGroupName: logGroupName,
		ctx:          ctx,
	}, nil
}

func (p *EC2Provider) findInstanceLogGroup(ctx context.Context, instanceID string) (string, error) {
	paginator := cloudwatchlogs.NewDescribeLogGroupsPaginator(p.logsClient, &cloudwatchlogs.DescribeLogGroupsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", err
		}
		for _, lg := range page.LogGroups {
			name := aws.ToString(lg.LogGroupName)
			if strings.Contains(name, instanceID) {
				return name, nil
			}
		}
	}
	return "", nil
}

func (f *streamingEC2LogFile) Read(p []byte) (int, error) {
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

func (f *streamingEC2LogFile) fetchNextPage() error {
	if f.logGroupName == "" {
		f.fullyLoaded = true
		return nil
	}

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(f.logGroupName),
		Limit:        aws.Int32(100),
	}
	if f.nextToken != nil {
		input.NextToken = f.nextToken
	} else {
		// First request: get last hour
		input.StartTime = aws.Int64(time.Now().Add(-1 * time.Hour).UnixMilli())
	}

	resp, err := f.client.FilterLogEvents(f.ctx, input)
	if err != nil {
		return err
	}

	// Add header on first page
	if f.nextToken == nil && len(f.buffer) == 0 {
		f.buffer = append(f.buffer, []byte(fmt.Sprintf("# Log group: %s\n\n", f.logGroupName))...)
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
		if len(f.buffer) == 0 || (len(resp.Events) == 0 && f.nextToken == nil) {
			f.buffer = append(f.buffer, []byte("# No events in the last hour\n")...)
		}
	}

	return nil
}

func (f *streamingEC2LogFile) Close() error {
	f.buffer = nil
	return nil
}

func (f *streamingEC2LogFile) Size() int64 {
	return -1 // Unknown size for streaming
}
