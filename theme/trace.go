package theme

import (
	"hash/fnv"
	"strconv"
)

// traceTagColors is a small, fixed rotation of ANSI-256 codes used to
// badge a trace row's originating [namespace/service]. Deliberately
// disjoint from every code Palette() assigns semantic meaning to
// (33/39/75/105 warp-blue family, 208/173/214/215 weft-gold family,
// 67 comment, 68 markdown rule, 209 error, 2 diff-add) — a service
// badge sharing a color with e.g. Error would visually misread as
// something it isn't. Spans distinct hue families (rose, sky, violet,
// olive, turquoise, orchid, amber, lime) so adjacent badges in a
// fanned-out trace stay visually separable.
var traceTagColors = []int{197, 81, 141, 148, 51, 213, 172, 118}

// TraceTag renders a colored "[namespace/service] " badge for a
// trace row, ready to prepend to the row's raw text. The color is a
// deterministic hash of namespace+service, stable across process
// restarts (fnv, not hash/maphash, which randomizes per-process) so
// the same service gets the same badge color consistently within and
// across trace queries. Empty namespace/service (a row whose shard
// query somehow didn't carry origin metadata) renders as "[unknown]"
// rather than a blank or malformed "[/]" tag.
func TraceTag(namespace, service string) string {
	label := namespace + "/" + service
	if namespace == "" && service == "" {
		label = "unknown"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	code := traceTagColors[h.Sum32()%uint32(len(traceTagColors))]
	return "\x1b[38;5;" + strconv.Itoa(code) + "m[" + label + "] \x1b[0m"
}
