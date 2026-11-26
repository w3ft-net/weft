package dashboard

import (
	"fmt"
	"strings"

	dashstore "github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/shape"
)

// Readers is cronauthor.Service.Readers over st: the dashboards with a numeric
// tile on the cron at path (<namespace>/<name>), as the paths to show, so that a
// cron can't be made text out from under them.
func Readers(st dashstore.Store) func(owner shape.Added, path string) ([]string, error) {
	return func(owner shape.Added, path string) ([]string, error) {
		list, err := st.List(owner)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, d := range list {
			spec, err := Parse(d.YAMLBody)
			if err != nil {
				continue
			}
			if kinds := NumericTiles(spec, path); len(kinds) > 0 {
				out = append(out, fmt.Sprintf("/etc/dashboards/%s.yaml (%s)", d.Name, strings.Join(kinds, ", ")))
			}
		}
		return out, nil
	}
}
