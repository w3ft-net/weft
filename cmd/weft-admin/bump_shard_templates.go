package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// bumpShardTemplatesCmd points every configured shard launch template at the
// freshly-built AMI the deploy published to /<app>/ami/latest. It runs on the
// coordinator (under the instance role) as a run_remote post_deploy step, just
// before roll-all — so the fleet relaunches onto a live AMI rather than the
// stale one Terraform first stamped (lifecycle ignore_changes = image_id).
//
// It creates a new launch template version from $Latest with only ImageId
// overridden, so all other launch settings carry forward. The provisioner
// launches Version="$Latest", so the bump takes effect on the next launch.
func bumpShardTemplatesCmd(args []string) {
	fs := flag.NewFlagSet("bump-shard-templates", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "bump-shard-templates takes no positional arguments")
		os.Exit(2)
	}

	ctx := context.Background()

	templates := shardLaunchTemplateIDs()
	if len(templates) == 0 {
		log.Fatalf("no shard launch templates configured (SHARD_*_LAUNCH_TEMPLATE unset)")
	}

	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	// Resolve the AMI the deploy just published.
	amiParam := fmt.Sprintf("/%s/ami/latest", appName())
	param, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(amiParam),
	})
	if err != nil {
		log.Fatalf("read %s: %v", amiParam, err)
	}
	amiID := strings.TrimSpace(aws.ToString(param.Parameter.Value))
	if amiID == "" {
		log.Fatalf("%s is empty", amiParam)
	}
	fmt.Printf("Pointing %d shard launch template(s) at %s (%s)\n", len(templates), amiID, amiParam)

	ec2c := ec2.NewFromConfig(cfg)
	for _, tier := range sortedTiers(templates) {
		ltID := templates[tier]
		out, err := ec2c.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
			LaunchTemplateId:   aws.String(ltID),
			SourceVersion:      aws.String("$Latest"),
			LaunchTemplateData: &ec2types.RequestLaunchTemplateData{ImageId: aws.String(amiID)},
		})
		if err != nil {
			log.Fatalf("bump %s template %s: %v", tier, ltID, err)
		}
		fmt.Printf("  %-6s %s -> version %d\n", tier, ltID, aws.ToInt64(out.LaunchTemplateVersion.VersionNumber))
	}
	fmt.Println("Shard launch templates updated.")
}
