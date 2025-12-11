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

	parts := strings.Split(path, "/")

	// Category level: list items
	if len(parts) == 1 {
		switch parts[0] {
		case "users":
			return p.listUsers(ctx)
		case "roles":
			return p.listRoles(ctx)
		case "policies":
			return p.listPolicies(ctx)
		case "groups":
			return p.listGroups(ctx)
		}
	}

	// Item level: list files for that item
	if len(parts) == 2 {
		switch parts[0] {
		case "users":
			return p.listUserFiles(ctx)
		case "roles":
			return p.listRoleFiles(ctx)
		case "groups":
			return p.listGroupFiles(ctx)
		}
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
				Name:  aws.ToString(user.UserName),
				IsDir: true,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listUserFiles(ctx context.Context) ([]Entry, error) {
	return []Entry{
		{Name: "info.json", IsDir: false},
		{Name: "policies.json", IsDir: false},
		{Name: "groups.json", IsDir: false},
	}, nil
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
				Name:  aws.ToString(role.RoleName),
				IsDir: true,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listRoleFiles(ctx context.Context) ([]Entry, error) {
	return []Entry{
		{Name: "info.json", IsDir: false},
		{Name: "policies.json", IsDir: false},
	}, nil
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
				Name:  aws.ToString(group.GroupName),
				IsDir: true,
			})
		}
	}

	return entries, nil
}

func (p *IAMProvider) listGroupFiles(ctx context.Context) ([]Entry, error) {
	return []Entry{
		{Name: "info.json", IsDir: false},
		{Name: "policies.json", IsDir: false},
		{Name: "members.json", IsDir: false},
	}, nil
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

	// policies/<name>.json (policies stay flat)
	if len(parts) == 2 && parts[0] == "policies" {
		name := strings.TrimSuffix(parts[1], ".json")
		return p.getPolicyInfo(ctx, name)
	}

	// users/<name>/<file>.json, roles/<name>/<file>.json, groups/<name>/<file>.json
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	category := parts[0]
	name := parts[1]
	file := parts[2]

	switch category {
	case "users":
		switch file {
		case "info.json":
			return p.getUserInfo(ctx, name)
		case "policies.json":
			return p.getUserPolicies(ctx, name)
		case "groups.json":
			return p.getUserGroups(ctx, name)
		}
	case "roles":
		switch file {
		case "info.json":
			return p.getRoleInfo(ctx, name)
		case "policies.json":
			return p.getRolePolicies(ctx, name)
		}
	case "groups":
		switch file {
		case "info.json":
			return p.getGroupInfo(ctx, name)
		case "policies.json":
			return p.getGroupPolicies(ctx, name)
		case "members.json":
			return p.getGroupMembers(ctx, name)
		}
	}

	return nil, fmt.Errorf("unknown path: %s", path)
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

func (p *IAMProvider) getUserPolicies(ctx context.Context, userName string) ([]byte, error) {
	var policies []string

	// Get attached policies
	attachedResp, err := p.client.ListAttachedUserPolicies(ctx, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err == nil {
		for _, policy := range attachedResp.AttachedPolicies {
			policies = append(policies, aws.ToString(policy.PolicyArn))
		}
	}

	// Get inline policies
	inlineResp, err := p.client.ListUserPolicies(ctx, &iam.ListUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err == nil {
		for _, policyName := range inlineResp.PolicyNames {
			policies = append(policies, "inline:"+policyName)
		}
	}

	return json.MarshalIndent(policies, "", "  ")
}

func (p *IAMProvider) getUserGroups(ctx context.Context, userName string) ([]byte, error) {
	var groups []string

	resp, err := p.client.ListGroupsForUser(ctx, &iam.ListGroupsForUserInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return nil, err
	}

	for _, group := range resp.Groups {
		groups = append(groups, aws.ToString(group.GroupName))
	}

	return json.MarshalIndent(groups, "", "  ")
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

func (p *IAMProvider) getRolePolicies(ctx context.Context, roleName string) ([]byte, error) {
	var policies []string

	// Get attached policies
	attachedResp, err := p.client.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err == nil {
		for _, policy := range attachedResp.AttachedPolicies {
			policies = append(policies, aws.ToString(policy.PolicyArn))
		}
	}

	// Get inline policies
	inlineResp, err := p.client.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err == nil {
		for _, policyName := range inlineResp.PolicyNames {
			policies = append(policies, "inline:"+policyName)
		}
	}

	return json.MarshalIndent(policies, "", "  ")
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

func (p *IAMProvider) getGroupPolicies(ctx context.Context, groupName string) ([]byte, error) {
	var policies []string

	// Get attached policies
	attachedResp, err := p.client.ListAttachedGroupPolicies(ctx, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String(groupName),
	})
	if err == nil {
		for _, policy := range attachedResp.AttachedPolicies {
			policies = append(policies, aws.ToString(policy.PolicyArn))
		}
	}

	// Get inline policies
	inlineResp, err := p.client.ListGroupPolicies(ctx, &iam.ListGroupPoliciesInput{
		GroupName: aws.String(groupName),
	})
	if err == nil {
		for _, policyName := range inlineResp.PolicyNames {
			policies = append(policies, "inline:"+policyName)
		}
	}

	return json.MarshalIndent(policies, "", "  ")
}

func (p *IAMProvider) getGroupMembers(ctx context.Context, groupName string) ([]byte, error) {
	var members []string

	resp, err := p.client.GetGroup(ctx, &iam.GetGroupInput{
		GroupName: aws.String(groupName),
	})
	if err != nil {
		return nil, err
	}

	for _, user := range resp.Users {
		members = append(members, aws.ToString(user.UserName))
	}

	return json.MarshalIndent(members, "", "  ")
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

	// policies/<name>.json (flat structure)
	if len(parts) == 2 && parts[0] == "policies" && strings.HasSuffix(parts[1], ".json") {
		return &Entry{Name: parts[1], IsDir: false, Size: 4096}, nil
	}

	// users/<name>, roles/<name>, groups/<name> directories
	if len(parts) == 2 {
		switch parts[0] {
		case "users", "roles", "groups":
			return &Entry{Name: parts[1], IsDir: true}, nil
		}
	}

	// users/<name>/<file>.json, roles/<name>/<file>.json, groups/<name>/<file>.json
	if len(parts) == 3 && strings.HasSuffix(parts[2], ".json") {
		return &Entry{Name: parts[2], IsDir: false, Size: 4096}, nil
	}

	return nil, fmt.Errorf("path not found: %s", path)
}
