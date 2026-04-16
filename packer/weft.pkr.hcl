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

variable "base_ami_id" {
  type    = string
  default = ""
}

variable "source_ami_filter" {
  type    = string
  default = "debian-12-base-*"
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

variable "github_token" {
  type      = string
  default   = ""
  sensitive = true
}

variable "codeartifact_auth_token" {
  type      = string
  default   = ""
  sensitive = true
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
  ami_name = "${var.app_name}-coordinator-${var.git_sha}-${local.ts}"
}

source "amazon-ebs" "weft" {
  ami_name      = local.ami_name
  instance_type = var.instance_type
  region        = var.region

  assume_role {
    role_arn     = "arn:aws:iam::${var.assume_role_account_id}:role/${var.assume_role_name}"
    session_name = "packer-weft-build"
  }

  source_ami = var.base_ami_id != "" ? var.base_ami_id : null

  dynamic "source_ami_filter" {
    for_each = var.base_ami_id == "" ? [1] : []
    content {
      filters = {
        name                = var.source_ami_filter
        root-device-type    = "ebs"
        virtualization-type = "hvm"
        architecture        = "arm64"
      }
      most_recent = true
      owners      = ["self"]
    }
  }

  ssh_username         = "admin"
  ssh_timeout          = "30m"
  ssh_read_write_timeout = "30m"

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

  # Upload weft source (kevcons creates this tarball)
  provisioner "file" {
    source      = "/tmp/app-source.tar.gz"
    destination = "/tmp/app-source.tar.gz"
  }

  # Build binaries on the instance (Go, gcc, module cache pre-baked in base AMI)
  provisioner "shell" {
    environment_vars = [
      "GOPATH=/tmp/go",
      "GOMODCACHE=/opt/app/gomodcache",
      "HOME=/home/admin",
      "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      "GOPRIVATE=github.com/kevlarcon/*",
      "GONOSUMCHECK=github.com/kevlarcon/*",
      "GONOSUMDB=github.com/kevlarcon/*",
    ]
    inline = [
      "cd /tmp && tar xzf /tmp/app-source.tar.gz",
      "sed -i 's|=> ../warp|=> /tmp/warp|' /tmp/weft/go.mod",
      "cd /tmp/weft && CGO_ENABLED=1 go build -o /tmp/weft-coordinator ./cmd/coordinator",
      "cd /tmp/weft && CGO_ENABLED=1 go build -o /tmp/weft-shard ./cmd/shard",
      "cd /tmp/weft && CGO_ENABLED=1 go build -o /tmp/weft-compute ./cmd/compute",
      "cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/weft-api ./cmd/api",
      "cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/weftd ./cmd/weftd",
      "sudo mv /tmp/weft-coordinator /tmp/weft-shard /tmp/weft-compute /tmp/weft-api /opt/app/bin/",
      "sudo mv /tmp/weftd /usr/local/bin/weftd",
      "sudo chmod +x /opt/app/bin/weft-*",
      "sudo chown app:app /opt/app/bin/weft-*",
      # Clean up source and build artifacts (keep Go + gcc for future rebuilds if needed)
      "sudo rm -rf /tmp/weft /tmp/warp /tmp/app-source.tar.gz /tmp/go",
    ]
  }

  # Copy startup scripts
  provisioner "file" {
    source      = "deploy/startup.sh"
    destination = "/tmp/startup.sh"
  }

  provisioner "file" {
    source      = "deploy/fetch-config.sh"
    destination = "/tmp/fetch-config.sh"
  }

  provisioner "file" {
    source      = "deploy/start-role.sh"
    destination = "/tmp/start-role.sh"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/startup.sh /tmp/fetch-config.sh /tmp/start-role.sh /opt/app/scripts/",
      "sudo chmod +x /opt/app/scripts/startup.sh /opt/app/scripts/fetch-config.sh /opt/app/scripts/start-role.sh",
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

  provisioner "file" {
    source      = "deploy/weft-api.service"
    destination = "/tmp/weft-api.service"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/weft-startup.service /tmp/weft-coordinator.service /tmp/weft-shard.service /tmp/weft-compute.service /tmp/weft-api.service /etc/systemd/system/",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable weft-startup",
    ]
  }

  # CloudWatch agent config (agent itself is pre-installed in base AMI)
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

  # Manifest for kevcons to read the AMI ID
  post-processor "manifest" {
    output     = var.manifest_output != "" ? var.manifest_output : "manifest.json"
    strip_path = true
  }
}
