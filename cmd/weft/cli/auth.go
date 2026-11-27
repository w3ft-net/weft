package cli

import (
	"encoding/base64"
	"errors"
	"os"

	"github.com/w3ft-net/weft/shell"
)

// The variables the api reads its Basic credential from (ext/identity/basic
// names them too; core cannot import it).
const (
	basicUserEnv     = "WEFT_BASIC_AUTH_USER"
	basicPasswordEnv = "WEFT_BASIC_AUTH_PASSWORD"
)

// Basic is the open build's AuthProvider: the one HTTP Basic credential, read
// from the environment variables the api reads it from.
type Basic struct{}

func (Basic) Credential(*shell.Config) (string, error) {
	user, pass := os.Getenv(basicUserEnv), os.Getenv(basicPasswordEnv)
	if user == "" || pass == "" {
		return "", errors.New("no credential: set " + basicUserEnv + " and " + basicPasswordEnv + ", the HTTP Basic credential the api was started with")
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
}
