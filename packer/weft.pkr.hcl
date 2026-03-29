packer {
  required_plugins {
    amazon = {
      version = ">= 1.2.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "app_name" {
  type    = string
  default = "weft"
}

variable "region" {
  type    = string
  default = "us-east-2"
}

variable "source_ami_filter" {
  type    = string
  default = "debian-12-arm64-*"
}

variable "instance_type" {
  type    = string
  default = "t4g.small"
}

variable "git_sha" {
  type    = string
  default = "dev"
}

variable "source_path" {
  type    = string
  default = ""
}

variable "timestamp" {
  type    = string
  default = ""
}

variable "manifest_output" {
  type    = string
  default = ""
}

variable "assume_role_account_id" {
  type        = string
  description = "AWS account ID for kevlar-weft"
  default     = "220664822695" # kevlar-weft
}

variable "assume_role_name" {
  type    = string
  default = "TerragruntCrossAccountRole"
}

locals {
  ts       = var.timestamp != "" ? var.timestamp : formatdate("YYYYMMDDhhmmss", timestamp())
  ami_name = "${var.app_name}-${var.git_sha}-${local.ts}"
}

source "amazon-ebs" "weft" {
  ami_name      = local.ami_name
  instance_type = var.instance_type
  region        = var.region

  assume_role {
    role_arn     = "arn:aws:iam::${var.assume_role_account_id}:role/${var.assume_role_name}"
    session_name = "packer-weft-build"
  }

  source_ami_filter {
    filters = {
      name                = var.source_ami_filter
      root-device-type    = "ebs"
      virtualization-type = "hvm"
      architecture        = "arm64"
    }
    most_recent = true
    owners      = ["136693071363"] # Debian official
  }

  ssh_username = "admin"

  tags = {
    Name      = local.ami_name
    App       = var.app_name
    GitSHA    = var.git_sha
    Created   = local.ts
    ManagedBy = "packer"
  }
}

build {
  sources = ["source.amazon-ebs.weft"]

  # Create app user and directories
  provisioner "shell" {
    inline = [
      "sudo useradd -r -m -d /opt/app -s /bin/false app || true",
      "sudo mkdir -p /opt/app/bin /opt/app/scripts /opt/app/logs /opt/app/data",
      "sudo chown -R app:app /opt/app",
    ]
  }

  # Copy binaries (pre-compiled for linux/arm64)
  provisioner "file" {
    source      = "bin/weft-coordinator"
    destination = "/tmp/weft-coordinator"
  }

  provisioner "file" {
    source      = "bin/weft-shard"
    destination = "/tmp/weft-shard"
  }

  provisioner "file" {
    source      = "bin/weft-compute"
    destination = "/tmp/weft-compute"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/weft-coordinator /tmp/weft-shard /tmp/weft-compute /opt/app/bin/",
      "sudo chmod +x /opt/app/bin/weft-*",
      "sudo chown app:app /opt/app/bin/weft-*",
    ]
  }

  # Copy startup script
  provisioner "file" {
    source      = "deploy/startup.sh"
    destination = "/tmp/startup.sh"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/startup.sh /opt/app/scripts/startup.sh",
      "sudo chmod +x /opt/app/scripts/startup.sh",
    ]
  }

  # Copy systemd units
  provisioner "file" {
    source      = "deploy/weft-startup.service"
    destination = "/tmp/weft-startup.service"
  }

  provisioner "file" {
    source      = "deploy/weft-coordinator.service"
    destination = "/tmp/weft-coordinator.service"
  }

  provisioner "file" {
    source      = "deploy/weft-shard.service"
    destination = "/tmp/weft-shard.service"
  }

  provisioner "file" {
    source      = "deploy/weft-compute.service"
    destination = "/tmp/weft-compute.service"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/weft-startup.service /tmp/weft-coordinator.service /tmp/weft-shard.service /tmp/weft-compute.service /etc/systemd/system/",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable weft-startup",
    ]
  }

  # Install AWS CLI, CloudWatch agent, SSM agent
  provisioner "shell" {
    inline = [
      "sudo apt-get update -qq",
      "sudo apt-get install -y -qq curl jq unzip",
      # AWS CLI v2 (for startup.sh to read tags)
      "curl -sSL 'https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip' -o /tmp/awscli.zip",
      "unzip -q /tmp/awscli.zip -d /tmp",
      "sudo /tmp/aws/install",
      "rm -rf /tmp/aws /tmp/awscli.zip",
      # CloudWatch agent
      "curl -sSL https://s3.amazonaws.com/amazoncloudwatch-agent/debian/arm64/latest/amazon-cloudwatch-agent.deb -o /tmp/amazon-cloudwatch-agent.deb",
      "sudo dpkg -i /tmp/amazon-cloudwatch-agent.deb",
      "rm /tmp/amazon-cloudwatch-agent.deb",
      "sudo mkdir -p /opt/aws/amazon-cloudwatch-agent/etc",
      # SSM agent
      "curl -sSL https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/debian_arm64/amazon-ssm-agent.deb -o /tmp/amazon-ssm-agent.deb",
      "sudo dpkg -i /tmp/amazon-ssm-agent.deb",
      "sudo systemctl enable amazon-ssm-agent",
      "rm /tmp/amazon-ssm-agent.deb",
    ]
  }

  # CloudWatch agent config
  provisioner "file" {
    source      = "packer/cloudwatch-agent.json"
    destination = "/tmp/cloudwatch-agent.json"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/cloudwatch-agent.json /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json",
      "sudo systemctl enable amazon-cloudwatch-agent",
    ]
  }
}
