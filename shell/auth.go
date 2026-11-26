package shell

import "strings"

// Authorization is the Authorization header value for a credential. A bare
// token (a JWT, an opaque token) is a bearer credential; one that already names
// its scheme ("Basic dXNlcjpwYXNz") is sent as it is. A token never contains a
// space, so the two cannot be confused. This is how the CLI's AuthProvider hands
// the open build's HTTP Basic credential to the same clients that carry a
// bearer token elsewhere.
func Authorization(credential string) string {
	if strings.Contains(credential, " ") {
		return credential
	}
	return "Bearer " + credential
}
