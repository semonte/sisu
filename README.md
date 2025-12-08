# sisu

Browse AWS like a filesystem.

`grep`, `diff`, `cat`, `vim` your AWS resources. No more JSON wrangling with the AWS CLI.

## Installation

```bash
go install github.com/smonte/sisu@latest
```

### Requirements

**Linux:**
```bash
sudo apt install fuse    # Ubuntu/Debian
sudo yum install fuse    # RHEL/CentOS
```

## Usage

```bash
sisu
```

This mounts your AWS resources to `~/.sisu/mnt` and opens a shell. Type `exit` to unmount.

```
~/.sisu/mnt/
├── s3/
├── ssm/
├── vpc/
└── iam/
```

### Options

```bash
sisu --profile myprofile    # Use specific AWS profile
sisu --region us-west-2     # Use specific region
sisu --background           # Run as daemon
sisu stop                   # Unmount
```

## Examples

### Grep your infrastructure

```bash
# Find security groups allowing SSH
grep -r '"FromPort": 22' vpc/*/security-groups/

# Find all roles trusted by Lambda
grep -r "lambda.amazonaws.com" iam/roles/

# Find who has AdministratorAccess
grep -r "AdministratorAccess" iam/users/

# Search SSM for database connections
grep -r "postgres://" ssm/
```

### Diff environments

```bash
# Compare security groups
diff vpc/vpc-aaa/security-groups/sg-111.json vpc/vpc-bbb/security-groups/sg-222.json

# Compare IAM roles
diff iam/roles/prod-api.json iam/roles/staging-api.json
```

### Pipe to unix tools

```bash
# Pretty print with jq
cat iam/roles/my-role.json | jq '.AssumeRolePolicyDocument'

# Count resources
ls iam/roles/ | wc -l

# Fuzzy find with fzf
cat $(ls iam/roles/*.json | fzf)
```

### Edit SSM parameters

```bash
# Read
cat ssm/myapp/database-url

# Write
echo "postgres://prod-db:5432" > ssm/myapp/database-url

# Use your editor
vim ssm/myapp/config
```

### S3 with standard commands

```bash
# Copy files
cp local-file.txt s3/my-bucket/backup/

# Read logs
cat s3/my-bucket/logs/app.log | grep ERROR

# Delete
rm s3/my-bucket/old-file.txt
```

## Supported Services

| Service | Read | Write | Delete |
|---------|------|-------|--------|
| S3 | yes | yes | yes |
| SSM Parameter Store | yes | yes | yes |
| VPC (subnets, security groups, route tables) | yes | - | - |
| IAM (users, roles, policies, groups) | yes | - | - |

## Tips

- Use `ripgrep` (`rg`) instead of `grep -r` for faster parallel searches
- Results are cached for 5 minutes to reduce API calls
- S3 listings are limited to 100 items per directory

## License

MIT
