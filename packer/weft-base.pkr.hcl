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

locals {
  ts       = var.timestamp != "" ? var.timestamp : formatdate("YYYYMMDDhhmmss", timestamp())
  ami_name = "debian-12-base-weft-${var.version}-${local.ts}"
}

source "amazon-ebs" "weft-base" {
  ami_name      = local.ami_name
  instance_type = var.instance_type
  region        = var.region

  source_ami_filter {
    filters = {
      name                = "debian-12-arm64-*"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
      architecture        = "arm64"
    }
    most_recent = true
    owners      = ["136693071363"] # Debian official
  }

  ssh_username           = "admin"
  ssh_timeout            = "30m"
  ssh_read_write_timeout = "30m"

  ami_users = [var.share_with_account_id]

  tags = {
    Name      = local.ami_name
    App       = "weft"
    Created   = local.ts
    ManagedBy = "packer"
    Role      = "base"
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
      "GOPRIVATE=github.com/kevlarcon/*",
      "GONOSUMCHECK=github.com/kevlarcon/*",
      "GONOSUMDB=github.com/kevlarcon/*",
    ]
    inline = [
      "sudo mkdir -p /opt/app/gomodcache",
      "sudo chown -R admin:admin /opt/app/gomodcache",
      "mkdir -p /tmp/modcache-seed && cp /tmp/go.mod /tmp/go.sum /tmp/modcache-seed/",
      # Replace local warp path with a dummy module so go mod download doesn't fail
      "sed -i '/=> \\.\\.\\/warp/d' /tmp/modcache-seed/go.mod",
      "sed -i '/replace.*warp/d' /tmp/modcache-seed/go.mod",
      "cd /tmp/modcache-seed && go mod download -x 2>&1 | tail -5 || true",
      "sudo chown -R app:app /opt/app/gomodcache",
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

  post-processor "manifest" {
    output     = var.manifest_output != "" ? var.manifest_output : "manifest-base.json"
    strip_path = true
  }
}
