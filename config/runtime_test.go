package config

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
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

func TestOverlay_IgnoresUnknownAndBadValues(t *testing.T) {
	r := Defaults()
	orig := *r
	r.Overlay(map[string]string{
		"SOMEONE_ELSES_KEY": "whatever",
		keyLoginRPM:         "not-a-number",
		keyLoginBurst:       "", // empty string is a no-op, not an error
	})
	if r.LoginRPM != orig.LoginRPM {
		t.Errorf("bad int should not overwrite default: got %d, want %d", r.LoginRPM, orig.LoginRPM)
	}
	if r.LoginBurst != orig.LoginBurst {
		t.Errorf("empty string should not overwrite default: got %d, want %d", r.LoginBurst, orig.LoginBurst)
	}
}

func TestOverlay_AppliesRecognizedValues(t *testing.T) {
	r := Defaults()
	r.Overlay(map[string]string{
		keyLoginRPM:               "100",
		keyShellExecBodyMax:       "4194304",
		keyTrustedProxyCIDRs:      "172.16.0.0/12,10.0.0.0/8",
		keyShardQueryTimeoutSec:   "45",
		keyQueryDeadlineSec:       "90",
		keyIngestTokenBytesPerSec: "1048576",
		keyIngestTokenBurstBytes:  "4194304",
	})
	if r.LoginRPM != 100 {
		t.Errorf("LoginRPM: got %d, want 100", r.LoginRPM)
	}
	if r.ShellExecBodyMax != 4194304 {
		t.Errorf("ShellExecBodyMax: got %d, want 4194304", r.ShellExecBodyMax)
	}
	if r.TrustedProxyCIDRs != "172.16.0.0/12,10.0.0.0/8" {
		t.Errorf("TrustedProxyCIDRs: got %q", r.TrustedProxyCIDRs)
	}
	if r.ShardQueryTimeoutSeconds != 45 {
		t.Errorf("ShardQueryTimeoutSeconds: got %d, want 45", r.ShardQueryTimeoutSeconds)
	}
	if r.QueryDeadlineSeconds != 90 {
		t.Errorf("QueryDeadlineSeconds: got %d, want 90", r.QueryDeadlineSeconds)
	}
	if r.IngestTokenBytesPerSec != 1048576 {
		t.Errorf("IngestTokenBytesPerSec: got %d, want 1048576", r.IngestTokenBytesPerSec)
	}
	if r.IngestTokenBurstBytes != 4194304 {
		t.Errorf("IngestTokenBurstBytes: got %d, want 4194304", r.IngestTokenBurstBytes)
	}
}

func TestSSMSource_MissingParamIsEmpty(t *testing.T) {
	src := SSMSource{AppName: "weft", client: &fakeSSM{err: errors.New("ParameterNotFound: not found")}}
	r := Load(context.Background(), src)
	if r.LoginRPM != Defaults().LoginRPM {
		t.Errorf("expected defaults on missing param, got %d", r.LoginRPM)
	}
}

func TestSSMSource_AppliesBlob(t *testing.T) {
	src := SSMSource{AppName: "weft", client: &fakeSSM{value: `{"RATELIMIT_LOGIN_RPM": "99", "OTHER_TEAM_KEY": "ignored"}`}}
	r := Load(context.Background(), src)
	if r.LoginRPM != 99 {
		t.Errorf("LoginRPM: got %d, want 99", r.LoginRPM)
	}
	if r.LoginBurst != Defaults().LoginBurst {
		t.Errorf("LoginBurst should retain default")
	}
}

func TestSSMSource_InvalidJSONFallsBack(t *testing.T) {
	r := Load(context.Background(), SSMSource{AppName: "weft", client: &fakeSSM{value: `not-json`}})
	if r.LoginRPM != Defaults().LoginRPM {
		t.Errorf("expected defaults on bad JSON")
	}
}
