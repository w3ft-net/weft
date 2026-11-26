package cli

import (
	"encoding/base64"
	"errors"
	"os"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/shell"
)

// Basic is the open build's AuthProvider: the one HTTP Basic credential, read
// from the environment variables the api reads it from.
type Basic struct{}

func (Basic) Credential(*shell.Config) (string, error) {
	user, pass := os.Getenv(defaults.BasicUserEnv), os.Getenv(defaults.BasicPasswordEnv)
	if user == "" || pass == "" {
		return "", errors.New("no credential: set " + defaults.BasicUserEnv + " and " + defaults.BasicPasswordEnv + ", the HTTP Basic credential the api was started with")
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
}
