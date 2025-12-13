package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/semonte/sisu/internal/cache"
)

// EC2Provider provides access to AWS EC2 instances
type EC2Provider struct {
	ReadOnlyProvider
	client *ec2.Client
	cache  *cache.Cache
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
		client: ec2.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
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
		}, nil
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *EC2Provider) listInstances(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	var nextToken *string

	for {
		resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			NextToken: nextToken,
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
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	instanceID := parts[0]
	file := parts[1]

	switch file {
	case "info.json":
		return p.getInstanceInfo(ctx, instanceID)
	case "security-groups.json":
		return p.getSecurityGroups(ctx, instanceID)
	case "tags.json":
		return p.getTags(ctx, instanceID)
	}

	return nil, fmt.Errorf("unknown file: %s", file)
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
	return json.MarshalIndent(instance.SecurityGroups, "", "  ")
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

	parts := strings.Split(path, "/")

	// Instance directory
	if len(parts) == 1 {
		resp, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{parts[0]},
		})
		if err != nil || len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
			return nil, fmt.Errorf("instance not found: %s", parts[0])
		}
		return &Entry{Name: parts[0], IsDir: true}, nil
	}

	// Files
	if len(parts) == 2 {
		switch parts[1] {
		case "info.json", "security-groups.json", "tags.json":
			return &Entry{Name: parts[1], IsDir: false, Size: 4096}, nil
		}
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
