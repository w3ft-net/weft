package tenancy_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The guided tour is a hosted product feature. The open build compiles none of
// it, so the packages every build shares must not import the tour's packages or
// speak its vocabulary. A hosted deployment adds it through the adapter seams
// (Identity.ShellRan, notices.Register).
//
// Core directories are walked recursively; a directory listed here is core
// throughout.
var tourFreeDirs = []string{
	"api", "shell", "shellfs", "mounts", "alerting", "identity", "app", "notices",
	"tenancy/single", "cmd/weft", "cmd/wasm", "cmd/api", "cmd/coordinator",
}

// Packages only the tour uses.
var tourPackages = []string{"weft/coordinator/onboarding"}

// Identifiers the tour owns. "onboarding" alone is not one: a source is
// onboarded too.
var tourWords = regexp.MustCompile(`(?i)\btour\b|tour_advance|ShellAdvance|\blessons?\b|graduat|LoomCustomerSlug|KeyOnboardingGraduated`)

// Files in a core directory that are hosted by build tag or by name and may
// speak the tour. Each needs its reason.
var tourExceptions = map[string]string{}

func TestCoreCarriesNoTour(t *testing.T) {
	root := ".."
	var found []string
	for _, dir := range tourFreeDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if _, ok := tourExceptions[rel]; ok {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, p := range tourPackages {
				if strings.Contains(string(src), `"github.com/graybeardcorp/`+p+`"`) {
					found = append(found, rel+": imports "+p)
				}
			}
			for i, line := range strings.Split(string(src), "\n") {
				if w := tourWords.FindString(line); w != "" {
					found = append(found, rel+":"+strconv.Itoa(i+1)+": "+w)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) > 0 {
		t.Errorf("the tour is hosted-only, but core carries it; move it behind the adapter or reword:\n  %s", strings.Join(found, "\n  "))
	}
}
