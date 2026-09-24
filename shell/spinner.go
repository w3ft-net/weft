package shell

import "github.com/graybeardcorp/warp/pkg/shellcore"

// weftSpinnerStyle is weft's running-query indicator: a weft thread
// (the shuttle) bouncing top and bottom through the warp threads, cycling
// through the startup logo's weft (warm) and warp (cool) thread colors —
// see weftPalette for the shared 256-color derivation.
func weftSpinnerStyle() shellcore.SpinnerStyle {
	return shellcore.SpinnerStyle{
		Frames: []string{
			"│̅│̅", // │̅│̅
			"│̅┐̅", // │̅┐̅
			"│̲┘̲", // │̲┘̲
			"│̲│̲", // │̲│̲
			"└̲│̲", // └̲│̲
			"┌̅│̅", // ┌̅│̅
		},
		Colors:  []int{208, 209, 215, 214, 173, 74, 75, 39, 32, 33},
		Message: " weaving...",
	}
}
