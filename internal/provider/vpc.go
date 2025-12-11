package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/semonte/sisu/internal/cache"
)

// Debug controls whether VPC provider operations are logged
var Debug bool

// VPCProvider provides access to AWS VPCs
type VPCProvider struct {
	ReadOnlyProvider
	client *ec2.Client
	cache  *cache.Cache
}

// NewVPCProvider creates a new VPC provider
func NewVPCProvider(profile, region string) (*VPCProvider, error) {
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

	return &VPCProvider{
		client: ec2.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *VPCProvider) Name() string {
	return "vpc"
}

func (p *VPCProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *VPCProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list all VPCs
	if path == "" {
		return p.listVPCs(ctx)
	}

	parts := strings.SplitN(path, "/", 2)
	vpcID := parts[0]

	// VPC root: show available info files
	if len(parts) == 1 {
		return []Entry{
			{Name: "info.json", IsDir: false},
			{Name: "subnets", IsDir: true},
			{Name: "route-tables", IsDir: true},
			{Name: "security-groups", IsDir: true},
		}, nil
	}

	subpath := parts[1]

	switch {
	case subpath == "subnets":
		return p.listSubnets(ctx, vpcID)
	case subpath == "route-tables":
		return p.listRouteTables(ctx, vpcID)
	case subpath == "security-groups":
		return p.listSecurityGroups(ctx, vpcID)
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *VPCProvider) listVPCs(ctx context.Context) ([]Entry, error) {
	resp, err := p.client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(resp.Vpcs))
	for i, vpc := range resp.Vpcs {
		entries[i] = Entry{
			Name:  aws.ToString(vpc.VpcId),
			IsDir: true,
		}
	}

	return entries, nil
}

func (p *VPCProvider) listSubnets(ctx context.Context, vpcID string) ([]Entry, error) {
	resp, err := p.client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(resp.Subnets))
	for i, subnet := range resp.Subnets {
		entries[i] = Entry{
			Name:  aws.ToString(subnet.SubnetId) + ".json",
			IsDir: false,
		}
	}

	return entries, nil
}

func (p *VPCProvider) listRouteTables(ctx context.Context, vpcID string) ([]Entry, error) {
	resp, err := p.client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(resp.RouteTables))
	for i, rt := range resp.RouteTables {
		entries[i] = Entry{
			Name:  aws.ToString(rt.RouteTableId) + ".json",
			IsDir: false,
		}
	}

	return entries, nil
}

func (p *VPCProvider) listSecurityGroups(ctx context.Context, vpcID string) ([]Entry, error) {
	resp, err := p.client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(resp.SecurityGroups))
	for i, sg := range resp.SecurityGroups {
		entries[i] = Entry{
			Name:  aws.ToString(sg.GroupId) + ".json",
			IsDir: false,
		}
	}

	return entries, nil
}

func (p *VPCProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *VPCProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	if Debug {
		log.Printf("[vpc] Read: path=%q", path)
	}

	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	vpcID := parts[0]
	if Debug {
		log.Printf("[vpc] Read: vpcID=%q parts=%v", vpcID, parts)
	}

	// VPC info.json
	if len(parts) == 2 && parts[1] == "info.json" {
		return p.getVPCInfo(ctx, vpcID)
	}

	// Subnets, route tables, security groups
	if len(parts) == 3 {
		resourceType := parts[1]
		resourceFile := parts[2]

		if Debug {
			log.Printf("[vpc] Read: resourceType=%q resourceFile=%q", resourceType, resourceFile)
		}

		switch resourceType {
		case "subnets":
			return p.getSubnetInfo(ctx, resourceFile)
		case "route-tables":
			return p.getRouteTableInfo(ctx, resourceFile)
		case "security-groups":
			return p.getSecurityGroupInfo(ctx, resourceFile)
		}
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *VPCProvider) getVPCInfo(ctx context.Context, vpcID string) ([]byte, error) {
	resp, err := p.client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Vpcs) == 0 {
		return nil, fmt.Errorf("VPC not found: %s", vpcID)
	}

	return json.MarshalIndent(resp.Vpcs[0], "", "  ")
}

func (p *VPCProvider) getSubnetInfo(ctx context.Context, filename string) ([]byte, error) {
	subnetID := strings.TrimSuffix(filename, ".json")

	resp, err := p.client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: []string{subnetID},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Subnets) == 0 {
		return nil, fmt.Errorf("subnet not found: %s", subnetID)
	}

	return json.MarshalIndent(resp.Subnets[0], "", "  ")
}

func (p *VPCProvider) getRouteTableInfo(ctx context.Context, filename string) ([]byte, error) {
	rtID := strings.TrimSuffix(filename, ".json")

	resp, err := p.client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{rtID},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.RouteTables) == 0 {
		return nil, fmt.Errorf("route table not found: %s", rtID)
	}

	return json.MarshalIndent(resp.RouteTables[0], "", "  ")
}

func (p *VPCProvider) getSecurityGroupInfo(ctx context.Context, filename string) ([]byte, error) {
	sgID := strings.TrimSuffix(filename, ".json")

	resp, err := p.client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.SecurityGroups) == 0 {
		return nil, fmt.Errorf("security group not found: %s", sgID)
	}

	return json.MarshalIndent(resp.SecurityGroups[0], "", "  ")
}

func (p *VPCProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *VPCProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "vpc", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")
	vpcID := parts[0]

	// Check if VPC exists
	if len(parts) == 1 {
		resp, err := p.client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			VpcIds: []string{vpcID},
		})
		if err != nil || len(resp.Vpcs) == 0 {
			return nil, fmt.Errorf("VPC not found: %s", vpcID)
		}
		return &Entry{Name: parts[0], IsDir: true}, nil
	}

	// Subdirectories
	if len(parts) == 2 {
		switch parts[1] {
		case "info.json":
			// Size unknown until read, use placeholder that will be corrected by sisuFile.GetAttr
			return &Entry{Name: "info.json", IsDir: false, Size: 4096}, nil
		case "subnets", "route-tables", "security-groups":
			return &Entry{Name: parts[1], IsDir: true}, nil
		}
	}

	// Resource files
	if len(parts) == 3 && strings.HasSuffix(parts[2], ".json") {
		return &Entry{Name: parts[2], IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
