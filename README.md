# sisu ⚡

**Your AWS, as a filesystem.**

This:
```bash
grep -l "AdministratorAccess" iam/users/*/policies.json
```

Instead of this:
```bash
aws iam list-users --query 'Users[].UserName' --output text | \
  xargs -I{} sh -c 'aws iam list-attached-user-policies --user-name {} --query "AttachedPolicies[].PolicyArn" --output text' | \
  grep AdministratorAccess
```

## What is this? 🤔

sisu mounts AWS resources as a local filesystem. Use the tools you already know - `grep`, `cat`, `diff`, `vim` - instead of wrestling with JSON and the AWS CLI. Currently supports S3, SSM, IAM, VPC, Lambda, and EC2.


## Install 📦

```bash
go install github.com/semonte/sisu@latest
```

Requires FUSE:
```bash
sudo apt install fuse    # Ubuntu/Debian
sudo yum install fuse    # RHEL/CentOS
```

## Quick Start 🚀

```bash
sisu
```

You're in. Your AWS is now at your fingertips:

```
~/.sisu/mnt/
├── s3/
├── ssm/
├── vpc/
├── iam/
├── lambda/
└── ec2/
```

Type `exit` when done.

## The Good Stuff 🔥

### Find security issues in seconds

```bash
# Who has admin access?
grep -l "AdministratorAccess" iam/users/*/policies.json

# Security groups with SSH open
grep -r '"FromPort": 22' vpc/*/security-groups/

# Roles that Lambda can assume
grep -l "lambda.amazonaws.com" iam/roles/*/info.json

# Secrets in SSM?
grep -r "password" ssm/

# Lambda functions with secrets in env vars
grep -r "PASSWORD\|SECRET\|API_KEY" lambda/*/env.json

# Functions using deprecated runtimes
grep -r "python3.8\|nodejs16" lambda/*/config.json

# EC2 instances with public IPs
grep -r "PublicIpAddress" ec2/*/info.json

# Find stopped instances (wasting money?)
grep -r '"Name": "stopped"' ec2/*/info.json
```

### Diff your environments

```bash
# Why is prod broken but staging works?
diff iam/roles/prod-api/info.json iam/roles/staging-api/info.json

# Security group drift
diff vpc/vpc-prod/security-groups/sg-xxx.json vpc/vpc-staging/security-groups/sg-yyy.json

# Lambda config differences
diff lambda/my-func-prod/config.json lambda/my-func-staging/config.json
```

### Pipe to anything

```bash
# Pretty print with jq
cat iam/roles/my-role/info.json | jq '.AssumeRolePolicyDocument'

# Count your roles
ls iam/roles/ | wc -l

# Find untagged resources
cat vpc/vpc-xxx/info.json | jq 'select(.Tags == null)'

# List all Lambda runtimes in use
grep -h "Runtime" lambda/*/config.json | sort | uniq -c
```

### Edit SSM like a file

```bash
cat ssm/myapp/database-url                         # read
echo "postgres://prod:5432" > ssm/database-url     # write
vim ssm/myapp/config                               # edit
```

### S3, the unix way

```bash
cp local.txt s3/my-bucket/backup/
cat s3/my-bucket/logs/app.log | grep ERROR
rm s3/my-bucket/old-file.txt
```

## Options ⚙️

```bash
sisu --profile prod       # AWS profile
sisu --region us-west-2   # AWS region
sisu stop                 # Unmount
```

## What's Supported ✅

| Service | Read | Write | Delete |
|---------|:----:|:-----:|:------:|
| S3 | ✓ | ✓ | ✓ |
| SSM Parameter Store | ✓ | ✓ | ✓ |
| IAM (users, roles, policies, groups) | ✓ | - | - |
| VPC (subnets, security groups, routes) | ✓ | - | - |
| Lambda (config, policy, env vars) | ✓ | - | - |
| EC2 (instances, security groups, tags) | ✓ | - | - |

## Tips 💡

- Results are cached for 5 minutes
- S3 listings cap at 100 items per directory

## License 📄

MIT
