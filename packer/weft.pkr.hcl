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

  dynamic "assume_role" {
    for_each = var.assume_role_account_id != "" ? [1] : []
    content {
      role_arn     = "arn:aws:iam::${var.assume_role_account_id}:role/${var.assume_role_name}"
      session_name = "packer-weft-build"
    }
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
      "cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/weft-migrate ./cmd/weft-migrate",
      # weft-admin: the coordinator-resident CLI for operator/deploy actions
      # (e.g. `kevcons ssh --command 'sudo /opt/app/bin/weft-admin roll-shard
      # <id>'`). Cgo-free thin gRPC client, shipped on every AMI but only
      # meaningful when run on the coordinator instance.
      "cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/weft-admin ./cmd/weft-admin",
      # loom-producer is only invoked on the dedicated weft-loom-producer
      # host (a separate TF stack); coordinator/shard/compute hosts ship
      # the binary but never start the unit. Built static so the t4g.nano
      # producer can run it without dragging in cgo deps.
      "cd /tmp/weft && CGO_ENABLED=0 go build -o /tmp/loom-producer ./cmd/loom-producer",
      "sudo mv /tmp/weft-coordinator /tmp/weft-shard /tmp/weft-compute /tmp/weft-api /tmp/weft-admin /tmp/loom-producer /opt/app/bin/",
      "sudo mv /tmp/weftd /usr/local/bin/weftd",
      "sudo mv /tmp/weft-migrate /usr/local/bin/weft-migrate",
      "sudo chmod +x /usr/local/bin/weft-migrate",
      "sudo chmod +x /opt/app/bin/weft-* /opt/app/bin/loom-producer",
      "sudo chown app:app /opt/app/bin/weft-* /opt/app/bin/loom-producer",
      # Clean up source and build artifacts (keep Go + gcc for future rebuilds if needed)
      "sudo rm -rf /tmp/weft /tmp/warp /tmp/app-source.tar.gz /tmp/go",
    ]
  }

  # DuckDB CLI for direct shard inspection. The shard binary embeds
  # DuckDB via the Go binding (github.com/duckdb/duckdb-go/v2) which
  # statically links the engine; the CLI here is just for operators
  # opening /opt/app/data/shard.duckdb in --readonly mode after an
  # incident. Pinned to match the Go binding's underlying engine
  # version so the file format / extensions agree exactly.
  #
  # Keep DUCKDB_VERSION below in sync with go.mod's
  # github.com/duckdb/duckdb-go/v2 line. The binding's version string
  # encodes the engine: v2.10501.0 → engine 1.5.1.
  #
  # inline_shebang pins bash because the script uses `set -o pipefail`,
  # a bashism — packer's default `/bin/sh` is dash on Debian and
  # rejects it.
  provisioner "shell" {
    inline_shebang = "/bin/bash -e"
    inline = [
      <<-EOT
        set -euo pipefail
        DUCKDB_VERSION=v1.5.1
        # Map uname -m to DuckDB's release-asset arch labels.
        # DuckDB names assets duckdb_cli-linux-arm64.zip and
        # duckdb_cli-linux-amd64.zip — not aarch64/x86_64.
        case "$(uname -m)" in
          aarch64|arm64) DUCKDB_ARCH=linux-arm64 ;;
          x86_64|amd64)  DUCKDB_ARCH=linux-amd64 ;;
          *) echo "unsupported arch: $(uname -m)"; exit 1 ;;
        esac
        URL="https://github.com/duckdb/duckdb/releases/download/$${DUCKDB_VERSION}/duckdb_cli-$${DUCKDB_ARCH}.zip"
        echo "Installing DuckDB CLI from $${URL}"
        curl -fsSL -o /tmp/duckdb.zip "$${URL}"
        sudo unzip -qo /tmp/duckdb.zip -d /usr/local/bin/
        sudo chmod +x /usr/local/bin/duckdb
        rm /tmp/duckdb.zip
        # Smoke-test so a 404 / wrong-arch download fails the build
        # instead of a silent miss noticed only at incident time.
        /usr/local/bin/duckdb --version
      EOT
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

  # load-app-env-extra is weft's per-app hook for the universal
  # /usr/local/bin/load-app-env loader. Composes DB_DSN from the
  # DATABASE_* component env vars (DATABASE_HOST/NAME from the SSM
  # blob; DATABASE_USER/PASSWORD from the credentials secret) so
  # weft-coordinator / weft-api / weft-migrate get a full DSN string
  # in $DB_DSN without load-app-env having to know about postgres.
  # The loader checks /opt/app/scripts/load-app-env-extra at the end
  # of its run; the file's absence is a no-op.
  provisioner "file" {
    source      = "deploy/load-app-env-extra.sh"
    destination = "/tmp/load-app-env-extra"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/startup.sh /tmp/fetch-config.sh /tmp/start-role.sh /opt/app/scripts/",
      "sudo mv /tmp/load-app-env-extra /opt/app/scripts/load-app-env-extra",
      "sudo chmod +x /opt/app/scripts/startup.sh /opt/app/scripts/fetch-config.sh /opt/app/scripts/start-role.sh /opt/app/scripts/load-app-env-extra",
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

  # loom-producer assets — units, render script, streams.yaml. The
  # role unit is named weft-loom-producer.service so the AMI's
  # weft-startup.service can pick it up via the existing
  # `systemctl start weft-$ROLE` path when an instance is tagged
  # Role=loom-producer. Other roles never start it.
  provisioner "file" {
    source      = "deploy/loom-producer/weft-loom-producer.service"
    destination = "/tmp/weft-loom-producer.service"
  }

  provisioner "file" {
    source      = "deploy/loom-producer/weftd-loom@.service"
    destination = "/tmp/weftd-loom@.service"
  }

  provisioner "file" {
    source      = "deploy/loom-producer/render-weftd-config.sh"
    destination = "/tmp/render-weftd-config.sh"
  }

  provisioner "file" {
    source      = "deploy/loom-producer/streams.yaml"
    destination = "/tmp/streams.yaml"
  }

  provisioner "shell" {
    inline = [
      "sudo install -d -m 0755 /opt/app/scripts/loom-producer /etc/loom-producer",
      "sudo mv /tmp/weft-loom-producer.service /tmp/weftd-loom@.service /etc/systemd/system/",
      "sudo mv /tmp/render-weftd-config.sh /opt/app/scripts/loom-producer/render-weftd-config.sh",
      "sudo mv /tmp/streams.yaml /etc/loom-producer/streams.yaml",
      "sudo chmod +x /opt/app/scripts/loom-producer/render-weftd-config.sh",
      "sudo systemctl daemon-reload",
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
