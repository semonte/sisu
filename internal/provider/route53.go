package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/semonte/sisu/internal/cache"
)

// Route53Provider provides access to AWS Route 53
type Route53Provider struct {
	ReadOnlyProvider
	client *route53.Client
	cache  *cache.Cache
}

// NewRoute53Provider creates a new Route 53 provider
func NewRoute53Provider(profile string) (*Route53Provider, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	return &Route53Provider{
		client: route53.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *Route53Provider) Name() string {
	return "route53"
}

func (p *Route53Provider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *Route53Provider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list all hosted zones
	if path == "" {
		return p.listZones(ctx)
	}

	// Zone directory: show files
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return []Entry{
			{Name: "info.json", IsDir: false},
			{Name: "records.json", IsDir: false},
		}, nil
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *Route53Provider) listZones(ctx context.Context) ([]Entry, error) {
	cacheKey := "zones"
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.([]Entry), nil
	}

	var entries []Entry
	var marker *string

	for {
		resp, err := p.client.ListHostedZones(ctx, &route53.ListHostedZonesInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}

		for _, zone := range resp.HostedZones {
			// Use zone name, trim trailing dot
			name := strings.TrimSuffix(aws.ToString(zone.Name), ".")
			entries = append(entries, Entry{
				Name:  name,
				IsDir: true,
			})
		}

		if !resp.IsTruncated {
			break
		}
		marker = resp.NextMarker
	}

	p.cache.Set(cacheKey, entries)
	return entries, nil
}

func (p *Route53Provider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *Route53Provider) readUncached(ctx context.Context, path string) ([]byte, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	zoneName := parts[0]
	file := parts[1]

	// Find zone ID by name
	zoneID, err := p.findZoneID(ctx, zoneName)
	if err != nil {
		return nil, err
	}

	switch file {
	case "info.json":
		return p.getZoneInfo(ctx, zoneID)
	case "records.json":
		return p.getRecords(ctx, zoneID)
	}

	return nil, fmt.Errorf("unknown file: %s", file)
}

func (p *Route53Provider) findZoneID(ctx context.Context, zoneName string) (string, error) {
	cacheKey := "zoneid:" + zoneName
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(string), nil
	}

	var marker *string
	searchName := zoneName + "."

	for {
		resp, err := p.client.ListHostedZones(ctx, &route53.ListHostedZonesInput{
			Marker: marker,
		})
		if err != nil {
			return "", err
		}

		for _, zone := range resp.HostedZones {
			if aws.ToString(zone.Name) == searchName {
				// Zone ID is like /hostedzone/Z123, extract just the ID
				id := aws.ToString(zone.Id)
				id = strings.TrimPrefix(id, "/hostedzone/")
				p.cache.Set(cacheKey, id)
				return id, nil
			}
		}

		if !resp.IsTruncated {
			break
		}
		marker = resp.NextMarker
	}

	return "", fmt.Errorf("zone not found: %s", zoneName)
}

func (p *Route53Provider) getZoneInfo(ctx context.Context, zoneID string) ([]byte, error) {
	resp, err := p.client.GetHostedZone(ctx, &route53.GetHostedZoneInput{
		Id: aws.String(zoneID),
	})
	if err != nil {
		return nil, err
	}

	return json.MarshalIndent(resp, "", "  ")
}

func (p *Route53Provider) getRecords(ctx context.Context, zoneID string) ([]byte, error) {
	var allRecords []map[string]interface{}

	var startName *string
	var startType string

	for {
		input := &route53.ListResourceRecordSetsInput{
			HostedZoneId:    aws.String(zoneID),
			StartRecordName: startName,
		}
		if startType != "" {
			input.StartRecordType = types.RRType(startType)
		}

		resp, err := p.client.ListResourceRecordSets(ctx, input)
		if err != nil {
			return nil, err
		}

		for _, record := range resp.ResourceRecordSets {
			rec := map[string]interface{}{
				"Name": aws.ToString(record.Name),
				"Type": string(record.Type),
			}

			if record.TTL != nil {
				rec["TTL"] = *record.TTL
			}

			if len(record.ResourceRecords) > 0 {
				var values []string
				for _, rr := range record.ResourceRecords {
					values = append(values, aws.ToString(rr.Value))
				}
				rec["Values"] = values
			}

			if record.AliasTarget != nil {
				rec["AliasTarget"] = map[string]interface{}{
					"DNSName":              aws.ToString(record.AliasTarget.DNSName),
					"HostedZoneId":         aws.ToString(record.AliasTarget.HostedZoneId),
					"EvaluateTargetHealth": record.AliasTarget.EvaluateTargetHealth,
				}
			}

			allRecords = append(allRecords, rec)
		}

		if !resp.IsTruncated {
			break
		}
		startName = resp.NextRecordName
		startType = string(resp.NextRecordType)
	}

	return json.MarshalIndent(allRecords, "", "  ")
}

func (p *Route53Provider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *Route53Provider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "route53", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")

	// Zone directory
	if len(parts) == 1 {
		_, err := p.findZoneID(ctx, parts[0])
		if err != nil {
			return nil, fmt.Errorf("zone not found: %s", parts[0])
		}
		return &Entry{Name: parts[0], IsDir: true}, nil
	}

	// Files
	if len(parts) == 2 {
		switch parts[1] {
		case "info.json", "records.json":
			data, err := p.Read(ctx, path)
			size := int64(4096)
			if err == nil {
				size = int64(len(data))
			}
			return &Entry{Name: parts[1], IsDir: false, Size: size}, nil
		}
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
