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

sisu mounts AWS resources as a local filesystem. Use the tools you already know - `grep`, `cat`, `diff`, `vim` - instead of wrestling with JSON and the AWS CLI. Currently supports S3, SSM, IAM, VPC, Lambda, EC2, Secrets Manager, Route 53, and CloudWatch Logs.


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
│   │   ├── logs/
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

# Connect to an EC2 instance via SSM (no SSH keys needed!)
./default/us-east-1/ec2/i-abc123/connect

# View EC2 boot logs and kernel messages
cat default/us-east-1/ec2/i-abc123/console.log

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

# Grep recent logs for errors
grep -i "error" default/us-east-1/logs/aws/lambda/my-function/latest.log

# View all log groups
ls */us-east-1/logs/

# List log streams (shows 20 most recent)
ls default/us-east-1/logs/aws/lambda/my-function/

# View events from a specific stream
cat default/us-east-1/logs/aws/lambda/my-function/2024_01_15_abc123/events.log
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
| CloudWatch Logs | ✓ | - | - |

## How CloudWatch Logs Streaming Works 📜

Log stream `events.log` files are streamed lazily from AWS rather than loaded entirely into memory:

- **On-demand fetching**: Events are fetched in batches of 100 as you read through the file
- **Memory efficient**: Only fetched content is buffered, not the entire stream
- **Sequential reads**: Works with `cat`, `grep`, `head`, `less`

```bash
# Fetches only enough batches to find the match
grep "ERROR" .../my-stream/events.log

# Fetches just the first batch
head -50 .../my-stream/events.log

# Scroll through with on-demand loading
less .../my-stream/events.log

# Will fetch all events
cat .../my-stream/events.log | wc -l
```

**Note:** `tail` does not work correctly with streaming files because it seeks to the end of the file, but the actual file size is unknown until fully loaded. Use `cat ... | tail` as a workaround.

## EC2 Connect & Console 🖥️

Each EC2 instance has a `connect` script and `console.log`:

```bash
ls default/us-east-1/ec2/i-abc123/
# info.json  security-groups.json  tags.json  console.log  connect
```

**Connect via SSM** (no SSH keys, no public IP needed):
```bash
./default/us-east-1/ec2/i-abc123/connect
```
Requires [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html).

**View boot logs and kernel messages:**
```bash
cat default/us-east-1/ec2/i-abc123/console.log
```

## Tools That Pair Well 🔧

| Tool | What it does |
|------|--------------|
| [fzf](https://github.com/junegunn/fzf) | Fuzzy finder with preview |
| [jq](https://jqlang.github.io/jq/) | JSON query/transform |
| [difftastic](https://difftastic.wilfred.me.uk/) | Structural diff (understands JSON) |

```bash
# Browse and preview any resource interactively
find */global/iam/roles -name "info.json" | fzf --preview 'jq . {}'

# Find Lambda functions with high memory
jq -r 'select(.MemorySize > 512) | .FunctionName' */us-east-1/lambda/*/config.json

# Compare prod vs staging config
difft prod/us-east-1/lambda/api/config.json staging/us-east-1/lambda/api/config.json
```

## Ask AI About Your Infrastructure 🤖

Since it's just files, AI tools can read and analyze your AWS directly:

```bash
cd ~/.sisu/mnt && claude

"Find security groups that allow SSH from 0.0.0.0/0"
"Review IAM roles for overly permissive policies"
"Compare prod and staging Lambda configs"
```

## Tips 💡

- Results are cached for 5 minutes
- S3 listings cap at 100 items per directory
- CloudWatch Logs fetches events in batches of 100

## License 📄

MIT
