// Package theme holds weft's shared UI theming so the same definition
// backs both the interactive client (shell.ConfigureCore →
// core.SetPalette) and the server-side interpreter (coordinator.runLocal
// → interp.SetPalette). Keeping it in its own package — rather than
// exporting it from shell — lets the coordinator import the palette
// without pulling the client package, so a remotely-executed `cat /bin/x`
// renders in the exact weave the client shows for a locally-read file.
package theme

import "github.com/graybeardcorp/warp/pkg/syntax"

// Palette is weft's syntax-highlighting theme, derived from the startup
// logo (warp's pkg/shellcore/logo.go). The logo interlaces two thread
// families in 256-color ANSI:
//
//	warp threads (cool blues/indigos): 33 69 39 63 75 27 68 105 32 74
//	weft threads (warm oranges/golds): 208 173 214 209 215
//
// The mapping leans into that weave: code *structure* (commands,
// keywords, functions, builtins, flags) reads in the cool warp-blue
// family, while *literals* and operators read in the warm weft-gold
// family. Brand blue 33 and brand orange 208 — the most saturated thread
// of each family — anchor the two ends. Numbers keep the logo's indigo
// 105.
//
// Two deliberate departures from a pure logo derivation:
//   - Comments/quotes use 67 (a muted slate that lives just off the warp
//     family) so prose recedes instead of competing with live code.
//   - DiffAdd stays green (256-color 2): the logo carries no green, and
//     add/del legibility relies on the universal green/red contrast.
//     DiffDel uses the logo's salmon 209 (its reddest thread).
func Palette() syntax.Palette {
	return syntax.Palette{
		Reset: "\x1b[0m",

		// Code structure — warp-blue family.
		Command: "\x1b[1;38;5;33m", // bold brand blue
		Keyword: "\x1b[1;38;5;75m", // bold light blue
		Func:    "\x1b[1;38;5;39m", // bold azure
		Builtin: "\x1b[38;5;39m",   // azure
		Flag:    "\x1b[38;5;75m",   // light blue

		// Literals & operators — weft-gold family.
		String:    "\x1b[38;5;214m",   // gold
		StringEsc: "\x1b[38;5;215m",   // light peach
		Key:       "\x1b[1;38;5;214m", // bold gold
		Number:    "\x1b[38;5;105m",   // indigo (logo warp thread)
		Operator:  "\x1b[1;38;5;208m", // bold orange
		Punct:     "\x1b[38;5;173m",   // muted tan-orange

		// Prose / status.
		Comment: "\x1b[3;38;5;67m",  // italic muted slate
		Error:   "\x1b[1;38;5;209m", // bold salmon (logo weft thread)

		// Diffs — file/hunk chrome tinted to the warp family; add stays
		// semantic green, del takes the logo's salmon.
		DiffAdd:        "\x1b[38;5;2m",    // green (no logo green; kept for legibility)
		DiffDel:        "\x1b[38;5;209m",  // salmon
		DiffHunk:       "\x1b[38;5;75m",   // light blue
		DiffFileHeader: "\x1b[1;38;5;33m", // bold brand blue

		// Markdown prose — same weave: blue headings/links, gold emphasis
		// and code spans, orange list markers.
		MdHeading:    "\x1b[1;38;5;33m",  // bold brand blue
		MdEmphasis:   "\x1b[3;38;5;214m", // italic gold
		MdCodeSpan:   "\x1b[38;5;214m",   // gold (matches string)
		MdLinkText:   "\x1b[1;38;5;39m",  // bold azure
		MdLinkURL:    "\x1b[4;38;5;75m",  // underlined light blue
		MdListMarker: "\x1b[1;38;5;208m", // bold orange
		MdQuote:      "\x1b[3;38;5;67m",  // italic muted slate
		MdRule:       "\x1b[38;5;68m",    // steel blue
	}
}
