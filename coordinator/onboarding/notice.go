package onboarding

import (
	"sync"

	"github.com/graybeardcorp/weft/notices"
)

// GraduatedKey is the user_notices key a user gets on finishing the tour. The
// browser renders it as the graduation banner; the shell renders it as a
// notice like any other. Whichever surface the user reaches first delivers it.
const GraduatedKey = "onboarding_graduated"

var registerNotice sync.Once

// RegisterNotice teaches the notices registry the graduation notice. Every
// process that renders notices calls it, or a graduation row stays pending in
// that process: hosted's api and coordinator both do.
func RegisterNotice() {
	registerNotice.Do(func() {
		notices.Register(GraduatedKey, notices.KindSuccess, func(map[string]any) (string, string) {
			return "You've finished the tour",
				"You've seen the whole shell against Loom Inc's data. Your own org is ready when you are — connect a log source and the same pipelines work on your logs."
		})
	})
}
