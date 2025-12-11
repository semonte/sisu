package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/semonte/sisu/internal/cache"
)

// IAMProvider provides access to AWS IAM resources
type IAMProvider struct {
	ReadOnlyProvider
	client *iam.Client
	cache  *cache.Cache
}

// NewIAMProvider creates a new IAM provider
func NewIAMProvider(profile, region string) (*IAMProvider, error) {
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

	return &IAMProvider{
		client: iam.NewFromConfig(cfg),
		cache:  cache.New(5 * time.Minute),
	}, nil
}

func (p *IAMProvider) Name() string {
	return "iam"
}

func (p *IAMProvider) ReadDir(ctx context.Context, path string) ([]Entry, error) {
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

func (p *IAMProvider) readDirUncached(ctx context.Context, path string) ([]Entry, error) {
	// Root: list categories
	if path == "" {
		return []Entry{
			{Name: "users", IsDir: true},
			{Name: "roles", IsDir: true},
			{Name: "policies", IsDir: true},
			{Name: "groups", IsDir: true},
		}, nil
	}

	switch path {
	case "users":
		return p.listUsers(ctx)
	case "roles":
		return p.listRoles(ctx)
	case "policies":
		return p.listPolicies(ctx)
	case "groups":
		return p.listGroups(ctx)
	}

	return nil, fmt.Errorf("unknown path: %s", path)
}

func (p *IAMProvider) listUsers(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	paginator := iam.NewListUsersPaginator(p.client, &iam.ListUsersInput{})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, user := range page.Users {
			entries = append(entries, Entry{
				Name:  aws.ToString(user.UserName) + ".json",
				IsDir: false,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listRoles(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	paginator := iam.NewListRolesPaginator(p.client, &iam.ListRolesInput{})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, role := range page.Roles {
			entries = append(entries, Entry{
				Name:  aws.ToString(role.RoleName) + ".json",
				IsDir: false,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listPolicies(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	// Only list customer managed policies (not AWS managed)
	paginator := iam.NewListPoliciesPaginator(p.client, &iam.ListPoliciesInput{
		Scope: "Local",
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, policy := range page.Policies {
			entries = append(entries, Entry{
				Name:  aws.ToString(policy.PolicyName) + ".json",
				IsDir: false,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listGroups(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	paginator := iam.NewListGroupsPaginator(p.client, &iam.ListGroupsInput{})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, group := range page.Groups {
			entries = append(entries, Entry{
				Name:  aws.ToString(group.GroupName) + ".json",
				IsDir: false,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) Read(ctx context.Context, path string) ([]byte, error) {
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

func (p *IAMProvider) readUncached(ctx context.Context, path string) ([]byte, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	category := parts[0]
	filename := parts[1]
	name := strings.TrimSuffix(filename, ".json")

	switch category {
	case "users":
		return p.getUserInfo(ctx, name)
	case "roles":
		return p.getRoleInfo(ctx, name)
	case "policies":
		return p.getPolicyInfo(ctx, name)
	case "groups":
		return p.getGroupInfo(ctx, name)
	}

	return nil, fmt.Errorf("unknown category: %s", category)
}

func (p *IAMProvider) getUserInfo(ctx context.Context, userName string) ([]byte, error) {
	resp, err := p.client.GetUser(ctx, &iam.GetUserInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(resp.User, "", "  ")
}

func (p *IAMProvider) getRoleInfo(ctx context.Context, roleName string) ([]byte, error) {
	resp, err := p.client.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return nil, err
	}

	// Decode AssumeRolePolicyDocument if present
	if resp.Role.AssumeRolePolicyDocument != nil {
		decoded, err := url.QueryUnescape(aws.ToString(resp.Role.AssumeRolePolicyDocument))
		if err == nil {
			var policyDoc interface{}
			if json.Unmarshal([]byte(decoded), &policyDoc) == nil {
				// Convert to map to replace the encoded field
				var roleMap map[string]interface{}
				roleBytes, _ := json.Marshal(resp.Role)
				json.Unmarshal(roleBytes, &roleMap)
				roleMap["AssumeRolePolicyDocument"] = policyDoc
				return json.MarshalIndent(roleMap, "", "  ")
			}
		}
	}

	return json.MarshalIndent(resp.Role, "", "  ")
}

func (p *IAMProvider) getPolicyInfo(ctx context.Context, policyName string) ([]byte, error) {
	// First, list policies to find the ARN and default version
	var policyArn string
	var defaultVersionId string

	paginator := iam.NewListPoliciesPaginator(p.client, &iam.ListPoliciesInput{
		Scope: "Local",
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, policy := range page.Policies {
			if aws.ToString(policy.PolicyName) == policyName {
				policyArn = aws.ToString(policy.Arn)
				defaultVersionId = aws.ToString(policy.DefaultVersionId)
				break
			}
		}
		if policyArn != "" {
			break
		}
	}

	if policyArn == "" {
		return nil, fmt.Errorf("policy not found: %s", policyName)
	}

	// Get the policy document from the default version
	versionResp, err := p.client.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{
		PolicyArn: aws.String(policyArn),
		VersionId: aws.String(defaultVersionId),
	})
	if err != nil {
		return nil, err
	}

	// Decode the URL-encoded policy document
	if versionResp.PolicyVersion.Document != nil {
		decoded, err := url.QueryUnescape(aws.ToString(versionResp.PolicyVersion.Document))
		if err == nil {
			var policyDoc interface{}
			if json.Unmarshal([]byte(decoded), &policyDoc) == nil {
				// Return decoded and pretty-printed policy document
				return json.MarshalIndent(policyDoc, "", "  ")
			}
		}
	}

	return json.MarshalIndent(versionResp.PolicyVersion, "", "  ")
}

func (p *IAMProvider) getGroupInfo(ctx context.Context, groupName string) ([]byte, error) {
	resp, err := p.client.GetGroup(ctx, &iam.GetGroupInput{
		GroupName: aws.String(groupName),
	})
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(resp.Group, "", "  ")
}

func (p *IAMProvider) Stat(ctx context.Context, path string) (*Entry, error) {
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

func (p *IAMProvider) statUncached(ctx context.Context, path string) (*Entry, error) {
	if path == "" {
		return &Entry{Name: "iam", IsDir: true}, nil
	}

	parts := strings.Split(path, "/")

	// Category directories
	if len(parts) == 1 {
		switch parts[0] {
		case "users", "roles", "policies", "groups":
			return &Entry{Name: parts[0], IsDir: true}, nil
		}
		return nil, fmt.Errorf("unknown category: %s", parts[0])
	}

	// Resource files
	if len(parts) == 2 && strings.HasSuffix(parts[1], ".json") {
		return &Entry{Name: parts[1], IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
