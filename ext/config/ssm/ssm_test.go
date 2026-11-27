package ssm

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/w3ft-net/weft/config"
)

type fakeSSM struct {
	value string
	err   error
}

func (f *fakeSSM) GetParameter(ctx context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ssm.GetParameterOutput{
		Parameter: &ssmtypes.Parameter{Value: aws.String(f.value)},
	}, nil
}

func TestSource_MissingParamIsEmpty(t *testing.T) {
	src := Source{AppName: "weft", client: &fakeSSM{err: errors.New("ParameterNotFound: not found")}}
	r := config.Load(context.Background(), src)
	if r.LoginRPM != config.Defaults().LoginRPM {
		t.Errorf("expected defaults on missing param, got %d", r.LoginRPM)
	}
}

func TestSource_AppliesBlob(t *testing.T) {
	src := Source{AppName: "weft", client: &fakeSSM{value: `{"RATELIMIT_LOGIN_RPM": "99", "OTHER_TEAM_KEY": "ignored"}`}}
	r := config.Load(context.Background(), src)
	if r.LoginRPM != 99 {
		t.Errorf("LoginRPM: got %d, want 99", r.LoginRPM)
	}
	if r.LoginBurst != config.Defaults().LoginBurst {
		t.Errorf("LoginBurst should retain default")
	}
}

func TestSource_InvalidJSONFallsBack(t *testing.T) {
	r := config.Load(context.Background(), Source{AppName: "weft", client: &fakeSSM{value: `not-json`}})
	if r.LoginRPM != config.Defaults().LoginRPM {
		t.Errorf("expected defaults on bad JSON")
	}
}
