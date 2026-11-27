package config

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSMSource reads the single aggregated blob at /{AppName}/config/app, the
// parameter `kevcons config set` writes: a JSON object of string keys to string
// values. A parameter that does not exist is an empty source. Region is passed
// explicitly because the AWS SDK's default chain does not always resolve one from
// instance metadata.
type SSMSource struct {
	AppName string
	Region  string

	client ssmClient // tests substitute a fake
}

// ssmClient is a tiny interface so tests can substitute a fake.
type ssmClient interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

func (s SSMSource) Load(ctx context.Context) (map[string]string, error) {
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
