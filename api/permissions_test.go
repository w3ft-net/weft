//go:build !defaultshape

package api

import (
	"fmt"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"

	"github.com/graybeardcorp/saaskit/orgs"
)

func permsSeed(t *testing.T, s *store.Store, cust *store.Organization, role orgs.Role, emailSuffix string) (*store.User, string) {
	t.Helper()
	user, err := s.CreateVerifiedUser(fmt.Sprintf("%s-%d@example.com", emailSuffix, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	err = s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, role)
	if err != nil {
		t.Fatal(err)
	}
	jwt, _ := auth.Generate(user, "test-secret")
	return user, jwt
}
