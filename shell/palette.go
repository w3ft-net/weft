package shell

import "github.com/graybeardcorp/warp/pkg/syntax"

// weftPalette is weft's syntax-highlighting theme, derived from the
// startup logo (warp's pkg/shellcore/logo.go). The logo interlaces two
// thread families in 256-color ANSI:
//
//	warp threads (cool blues/indigos): 33 69 39 63 75 27 68 105 32 74
//	weft threads (warm oranges/golds): 208 173 214 209 215
//
// The mapping leans into that weave as three families:
//   - blue (structure): commands, functions, builtins, flags
//   - violet (grammar/literals): language keywords, numbers, escapes
//   - gold/orange (literals): strings, operators, punctuation
//
// Brand blue 33 and brand orange 208 — the most saturated thread of each
// outer family — anchor the ends. The violet family grows out of the
// logo's own indigo threads (63, 105): 105 anchors it as the number
// color, extended to 99/141 for the deeper and lighter violets the logo
// only hints at.
//
// Two deliberate departures from a pure logo derivation:
//   - Comments/quotes use 67 (a muted slate that lives just off the warp
//     family) so prose recedes instead of competing with live code.
//   - Diff add/del stay the universal green (2) / red (1): the logo
//     carries neither, but "added vs removed" must read unambiguously,
//     so semantics win over brand fidelity for those two lines.
func weftPalette() syntax.Palette {
	return syntax.Palette{
		Reset: "\x1b[0m",

		// Code structure — warp-blue family.
		Command: "\x1b[1;38;5;33m", // bold brand blue
		Func:    "\x1b[1;38;5;39m", // bold azure
		Builtin: "\x1b[38;5;39m",   // azure
		Flag:    "\x1b[38;5;75m",   // light blue

		// Grammar & numeric literals — violet family (logo indigo 63/105).
		Keyword:   "\x1b[1;38;5;141m", // bold light violet
		Number:    "\x1b[38;5;105m",   // indigo (logo warp thread)
		StringEsc: "\x1b[38;5;99m",    // medium purple

		// String literals & operators — weft-gold family.
		String:   "\x1b[38;5;214m",   // gold
		Key:      "\x1b[1;38;5;214m", // bold gold
		Operator: "\x1b[1;38;5;208m", // bold orange
		Punct:    "\x1b[38;5;173m",   // muted tan-orange

		// Prose / status.
		Comment: "\x1b[3;38;5;67m",  // italic muted slate
		Error:   "\x1b[1;38;5;209m", // bold salmon (logo weft thread)

		// Diffs — file/hunk chrome tinted to the warp family, but add/del
		// keep the universal semantic green/red. The logo carries neither
		// color; legibility of "added vs removed" trumps brand fidelity here.
		DiffAdd:        "\x1b[38;5;2m",    // green
		DiffDel:        "\x1b[38;5;1m",    // red
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
