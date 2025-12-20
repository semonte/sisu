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

sisu mounts AWS resources as a local filesystem. Use the tools you already know - `grep`, `cat`, `diff`, `vim` - instead of wrestling with JSON and the AWS CLI. Currently supports S3, SSM, IAM, VPC, Lambda, EC2, Secrets Manager, and Route 53.


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
├── default/              # AWS profile
│   ├── global/           # IAM, S3, Route 53 (region-independent)
│   │   ├── iam/
│   │   ├── route53/
│   │   └── s3/
│   ├── us-east-1/        # Regional services
│   │   ├── ec2/
│   │   ├── lambda/
│   │   ├── secrets/
│   │   ├── ssm/
│   │   └── vpc/
│   └── eu-west-1/
│       └── ...
├── prod/                 # Other profiles from ~/.aws/credentials
└── staging/
```

Type `exit` when done.

## The Good Stuff 🔥

### Explore your infrastructure

```bash
# Who has admin access?
grep -l "AdministratorAccess" */global/iam/users/*/policies.json

# Security groups with SSH open
grep -r '"FromPort": 22' */us-east-1/vpc/*/security-groups/

# Roles that Lambda can assume
grep -l "lambda.amazonaws.com" */global/iam/roles/*/info.json

# Secrets in SSM?
grep -r "password" */us-east-1/ssm/

# Lambda functions with secrets in env vars
grep -r "PASSWORD\|SECRET\|API_KEY" */us-east-1/lambda/*/env.json

# Functions using deprecated runtimes
grep -r "python3.8\|nodejs16" */*/lambda/*/config.json

# EC2 instances with public IPs
grep -r "PublicIpAddress" */*/ec2/*/info.json

# Find stopped instances (wasting money?)
grep -r '"Name": "stopped"' */*/ec2/*/info.json

# View all secrets
ls */us-east-1/secrets/

# Read a secret value
cat default/us-east-1/secrets/myapp/database/value

# List all DNS zones
ls */global/route53/

# View DNS records for a zone
cat default/global/route53/example.com/records.json

# Find all CNAME records
grep -r '"Type": "CNAME"' */global/route53/*/records.json
```

### Diff your environments

```bash
# Compare IAM roles between accounts
diff prod/global/iam/roles/api/info.json staging/global/iam/roles/api/info.json

# Security group drift between regions
diff default/us-east-1/vpc/vpc-xxx/security-groups/sg-xxx.json default/eu-west-1/vpc/vpc-yyy/security-groups/sg-yyy.json

# Lambda config differences
diff prod/us-east-1/lambda/my-func/config.json staging/us-east-1/lambda/my-func/config.json
```

### Pipe to anything

```bash
# Pretty print with jq
cat default/global/iam/roles/my-role/info.json | jq '.AssumeRolePolicyDocument'

# Count your roles
ls default/global/iam/roles/ | wc -l

# Find untagged resources
cat default/us-east-1/vpc/vpc-xxx/info.json | jq 'select(.Tags == null)'

# List all Lambda runtimes in use
grep -h "Runtime" */*/lambda/*/config.json | sort | uniq -c
```

### Edit SSM like a file

```bash
cat default/us-east-1/ssm/myapp/database-url          # read
echo "postgres://prod:5432" > default/us-east-1/ssm/database-url  # write
vim default/us-east-1/ssm/myapp/config                # edit
```

### S3, the unix way

```bash
cp local.txt default/global/s3/my-bucket/backup/
cat default/global/s3/my-bucket/logs/app.log | grep ERROR
rm default/global/s3/my-bucket/old-file.txt
```

## Options ⚙️

```bash
sisu                                    # Start at root
sisu --profile prod                     # Start in prod/
sisu --profile prod --region us-east-1  # Start in prod/us-east-1/
sisu stop                               # Unmount
sisu --debug                            # Debug logging
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
| Secrets Manager | ✓ | - | - |
| Route 53 (zones, records) | ✓ | - | - |

## Tips 💡

- Results are cached for 5 minutes
- S3 listings cap at 100 items per directory

## License 📄

MIT
