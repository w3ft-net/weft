package weft_test

import (
	"os"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposeServicesCannotFallThroughToRegistryPull pins the fix for a
// CI failure that cost three red runs before anyone read the right log
// line.
//
// docker-compose.yml gives six services the same `image: weft-server`,
// but only `compute` carries a `build:` section — the others reuse
// whatever that one build tagged. When the image is not present at
// `up` time, compose does not fail with "you forgot to build"; it
// treats `image:` as a registry reference and tries to pull, then
// reports:
//
//	api Error pull access denied for weft-server, repository does not
//	exist or may require 'docker login'
//
// which reads like a credentials problem and sends you looking at
// registry auth. Every integration job then dies in TestMain with
// "0 tests, 1 failure".
//
// A service that builds its own image is fine. A service that borrows
// one must declare `pull_policy: never`, so a missing image fails
// immediately and says so, instead of embarking on a doomed pull.
func TestComposeServicesCannotFallThroughToRegistryPull(t *testing.T) {
	for _, file := range []string{
		"docker-compose.yml",
		"docker-compose.localstack.yml",
		"docker-compose.cover.yml",
	} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var doc struct {
				Services map[string]struct {
					Image      string    `yaml:"image"`
					Build      yaml.Node `yaml:"build"`
					PullPolicy string    `yaml:"pull_policy"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}

			// Images this compose file builds itself. Anything else
			// (localstack, minio, postgres) is a real registry image and
			// *must* be pullable — the rule below is only about images
			// that exist solely because a sibling service built them.
			locallyBuilt := map[string]bool{}
			for _, svc := range doc.Services {
				if svc.Image != "" && !svc.Build.IsZero() {
					locallyBuilt[svc.Image] = true
				}
			}

			var offenders []string
			for name, svc := range doc.Services {
				if !locallyBuilt[svc.Image] || !svc.Build.IsZero() {
					continue
				}
				if svc.PullPolicy != "never" {
					offenders = append(offenders, name)
				}
			}
			sort.Strings(offenders)

			if len(offenders) > 0 {
				t.Fatalf("services %v borrow locally-built image %q without "+
					"pull_policy: never — a missing image becomes a registry "+
					"pull that fails as an auth error",
					offenders, doc.Services[offenders[0]].Image)
			}
		})
	}
}
