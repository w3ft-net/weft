package hostedapi_test

import (
	"testing"

	"github.com/graybeardcorp/weft/tenancy/hosted/hostedapi"
)

func TestJWTSecret_IsTheEnvironmentsOrADevDefault(t *testing.T) {
	t.Setenv("JWT_SECRET", "from-env")
	if got := hostedapi.JWTSecret(); got != "from-env" {
		t.Errorf("JWTSecret = %q, want the environment's", got)
	}
	t.Setenv("JWT_SECRET", "")
	if got := hostedapi.JWTSecret(); got != "dev-secret-change-in-production" {
		t.Errorf("JWTSecret = %q, want the dev default", got)
	}
}

func TestAPI_NeedsASecretAndConfiguresBilling(t *testing.T) {
	a := hostedapi.API()
	if a.Identity == nil || a.Tenancy == nil || a.Configure == nil || a.Start == nil || a.Serving == nil {
		t.Errorf("the hosted api adapter leaves a hook unset: %+v", a)
	}
}
