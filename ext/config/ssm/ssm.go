// Package ssm is a config.Source over AWS Systems Manager Parameter Store: the one
// aggregated blob `kevcons config set` writes, read by name from SSM.
package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/w3ft-net/weft/config"
)

// Source reads the single aggregated blob at /{AppName}/config/app, the
// parameter `kevcons config set` writes: a JSON object of string keys to string
// values. A parameter that does not exist is an empty source. Region is passed
// explicitly because the AWS SDK's default chain does not always resolve one from
// instance metadata.
type Source struct {
	AppName string
	Region  string

	client ssmClient // tests substitute a fake
}

// ssmClient is a tiny interface so tests can substitute a fake.
type ssmClient interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

func (s Source) Load(ctx context.Context) (map[string]string, error) {
	client := s.client
	if client == nil {
		opts := []func(*awsconfig.LoadOptions) error{}
		if s.Region != "" {
			opts = append(opts, awsconfig.WithRegion(s.Region))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("AWS config load failed: %w", err)
		}
		client = ssm.NewFromConfig(cfg)
	}
	name := fmt.Sprintf("/%s/config/app", s.AppName)
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: &name})
	if err != nil {
		if strings.Contains(err.Error(), "ParameterNotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("SSM GetParameter %s: %w", name, err)
	}
	if out == nil || out.Parameter == nil || out.Parameter.Value == nil {
		return nil, nil
	}
	var blob map[string]string
	if err := json.Unmarshal([]byte(*out.Parameter.Value), &blob); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", name, err)
	}
	return blob, nil
}

var _ config.Source = Source{}

// Kind is what a process is given to let WEFT_CONFIG_SOURCE=ssm name this source:
// the parameter is /{APP_NAME}/config/app, APP_NAME defaulting to "weft".
var Kind = config.Kind{
	Name: "ssm",
	New: func(region string) config.Source {
		app := os.Getenv("APP_NAME")
		if app == "" {
			app = "weft"
		}
		return Source{AppName: app, Region: region}
	},
}
