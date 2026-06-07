packer {
  required_plugins {
    amazon = {
      version = ">= 1.2.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "version" {
  type    = string
  default = "v1.0.0"
}

variable "region" {
  type    = string
  default = "us-east-2"
}

variable "instance_type" {
  type    = string
  default = "t4g.small"
}

variable "timestamp" {
  type    = string
  default = ""
}

variable "manifest_output" {
  type    = string
  default = ""
}

variable "share_with_account_id" {
  type    = string
  default = "220664822695"
}

# Per-arch base AMIs — kevcons build-base passes these through from
# .kevcons.yaml's build.architecture. Defaults keep the arm64 path
# (weft's production arch) working when packer is invoked directly.
variable "architecture" {
  type    = string
  default = "arm64"
}

variable "source_ami_filter" {
  type    = string
  default = "debian-12-arm64-*"
}

locals {
  ts       = var.timestamp != "" ? var.timestamp : formatdate("YYYYMMDDhhmmss", timestamp())
  ami_name = "debian-12-base-weft-${var.architecture}-${var.version}-${local.ts}"
}

source "amazon-ebs" "weft-base" {
  ami_name      = local.ami_name
  instance_type = var.instance_type
  region        = var.region

  source_ami_filter {
    filters = {
      name                = var.source_ami_filter
      root-device-type    = "ebs"
      virtualization-type = "hvm"
      architecture        = var.architecture
    }
    most_recent = true
    owners      = ["136693071363"] # Debian official
  }

  # Builder EC2 runs with this instance profile so the kevcons install
  # step below can pull from s3://kevlar-releases-.../kevcons/. The
  # bucket policy grants read to any org principal; the profile's role
  # has no explicit S3 policy. Managed in
  # infrastructure-live/management/kevcons-releases.
  iam_instance_profile = "packer-builder-kevcons-reader"

  ssh_username           = "admin"
  ssh_timeout            = "30m"
  ssh_read_write_timeout = "30m"

  ami_users = [var.share_with_account_id]

  tags = {
    Name         = local.ami_name
    App          = "weft"
    Architecture = var.architecture
    Created      = local.ts
    ManagedBy    = "packer"
    Role         = "base"
  }
}

build {
  sources = ["source.amazon-ebs.weft-base"]

  # Create app user and directories
  provisioner "shell" {
    inline = [
      "sudo useradd -r -m -d /opt/app -s /bin/false app || true",
      "sudo mkdir -p /opt/app/bin /opt/app/scripts /opt/app/logs /opt/app/data",
      "sudo chown -R app:app /opt/app",
    ]
  }

  # Install build tools (gcc/g++ for DuckDB CGO)
  provisioner "shell" {
    inline = [
      "sudo apt-get update -qq",
      "sudo apt-get install -y -qq gcc g++ git curl jq unzip",
    ]
  }

  # Install Go
  provisioner "shell" {
    inline = [
      "curl -sSL https://go.dev/dl/go1.26.0.linux-arm64.tar.gz -o /tmp/go.tar.gz",
      "sudo tar -C /usr/local -xzf /tmp/go.tar.gz",
      "rm /tmp/go.tar.gz",
      "echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh",
      "export PATH=$PATH:/usr/local/go/bin && go version",
    ]
  }

  # Pre-cache Go modules — upload go.mod/go.sum and run go mod download
  provisioner "file" {
    source      = "go.mod"
    destination = "/tmp/go.mod"
  }

  provisioner "file" {
    source      = "go.sum"
    destination = "/tmp/go.sum"
  }

  provisioner "shell" {
    environment_vars = [
      "GOPATH=/tmp/go",
      "GOMODCACHE=/opt/app/gomodcache",
      "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      "GOPRIVATE=github.com/graybeardcorp/*",
      "GONOSUMCHECK=github.com/graybeardcorp/*",
      "GONOSUMDB=github.com/graybeardcorp/*",
    ]
    inline = [
      "sudo mkdir -p /opt/app/gomodcache",
      "sudo chown -R admin:admin /opt/app/gomodcache",
      "mkdir -p /tmp/modcache-seed && cp /tmp/go.mod /tmp/go.sum /tmp/modcache-seed/",
      # Strip the sibling modules (warp, saaskit) from the seed go.mod so
      # go mod download pre-caches only the external deps. warp/saaskit are
      # provided from tarred source via a go workspace at app-build time
      # (see weft.pkr.hcl), so they don't belong in the module cache.
      "sed -i -E '/graybeardcorp\\/(warp|saaskit)/d' /tmp/modcache-seed/go.mod",
      "cd /tmp/modcache-seed && go mod download -x 2>&1 | tail -5 || true",
      # Cache stays admin-owned — the app build step runs as admin too
      "rm -rf /tmp/modcache-seed /tmp/go.mod /tmp/go.sum",
    ]
  }

  # Install AWS CLI v2
  provisioner "shell" {
    inline = [
      "curl -sSL 'https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip' -o /tmp/awscli.zip",
      "unzip -q /tmp/awscli.zip -d /tmp",
      "sudo /tmp/aws/install",
      "rm -rf /tmp/aws /tmp/awscli.zip",
    ]
  }

  # Install kevcons from the org-wide releases bucket. The builder EC2's
  # instance profile (see source block) gets read access via the bucket
  # policy on kevlar-releases; no explicit IAM needed. Pins whatever
  # version was LATEST at bake time into the AMI for its lifetime.
  # inline_shebang forces bash — Debian's /bin/sh is dash, which chokes
  # on `set -o pipefail`.
  provisioner "shell" {
    environment_vars = ["AWS_REGION=us-east-2"]
    inline_shebang   = "/bin/bash -e"
    inline = [
      "set -uo pipefail",
      "VERSION=$(aws s3 cp --no-progress s3://kevlar-releases-289202785125/kevcons/LATEST -)",
      "if [ -z \"$VERSION\" ]; then echo 'failed to resolve kevcons LATEST'; exit 1; fi",
      "echo \"installing kevcons $VERSION\"",
      "sudo aws s3 cp --no-progress \"s3://kevlar-releases-289202785125/kevcons/$VERSION/kevcons-linux-arm64\" /usr/local/bin/kevcons",
      "sudo chmod 0755 /usr/local/bin/kevcons",
      "/usr/local/bin/kevcons --version",
    ]
  }

  # Install the universal load-app-env loader. Canonical copy lives in
  # the infrastructure-catalog kevcons ansible role; we reach across to
  # the sibling checkout rather than duplicate the script. If the
  # sibling path is missing the bake fails loudly.
  provisioner "file" {
    source      = "../infrastructure-catalog/packer/ansible/roles/kevcons/files/load-app-env"
    destination = "/tmp/load-app-env"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/load-app-env /usr/local/bin/load-app-env",
      "sudo chown root:root /usr/local/bin/load-app-env",
      "sudo chmod 0755 /usr/local/bin/load-app-env",
    ]
  }

  # Install CloudWatch agent
  provisioner "shell" {
    inline = [
      "curl -sSL https://s3.amazonaws.com/amazoncloudwatch-agent/debian/arm64/latest/amazon-cloudwatch-agent.deb -o /tmp/amazon-cloudwatch-agent.deb",
      "sudo dpkg -i /tmp/amazon-cloudwatch-agent.deb",
      "rm /tmp/amazon-cloudwatch-agent.deb",
      "sudo mkdir -p /opt/aws/amazon-cloudwatch-agent/etc",
    ]
  }

  # Install SSM agent
  provisioner "shell" {
    inline = [
      "curl -sSL https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/debian_arm64/amazon-ssm-agent.deb -o /tmp/amazon-ssm-agent.deb",
      "sudo dpkg -i /tmp/amazon-ssm-agent.deb",
      "sudo systemctl enable amazon-ssm-agent",
      "rm /tmp/amazon-ssm-agent.deb",
    ]
  }

  # Inter-instance SSH: kevcons ssh --role <non-entrypoint> tunnels through
  # the coordinator using this bastion key pair. Mirrors the
  # infrastructure-catalog inter-instance-ssh ansible role used by django
  # apps. Every weft node gets the public key in authorized_keys and the
  # private key at /home/admin/.ssh/bastion_key — the coordinator is the
  # only one that normally uses the private key, but all nodes share the
  # same trust boundary so isolation per node isn't a meaningful gain.
  provisioner "file" {
    source      = "deploy/bastion_key"
    destination = "/tmp/bastion_key"
  }

  provisioner "file" {
    source      = "deploy/bastion_key.pub"
    destination = "/tmp/bastion_key.pub"
  }

  provisioner "shell" {
    inline = [
      "sudo mkdir -p /home/admin/.ssh",
      "sudo mv /tmp/bastion_key.pub /home/admin/.ssh/authorized_keys",
      "sudo mv /tmp/bastion_key    /home/admin/.ssh/bastion_key",
      "sudo chown -R admin:admin /home/admin/.ssh",
      "sudo chmod 700 /home/admin/.ssh",
      "sudo chmod 600 /home/admin/.ssh/authorized_keys /home/admin/.ssh/bastion_key",
    ]
  }

  post-processor "manifest" {
    output     = var.manifest_output != "" ? var.manifest_output : "manifest-base.json"
    strip_path = true
  }
}
