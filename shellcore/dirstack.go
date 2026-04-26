package shellcore

import (
	"fmt"

	"github.com/kevlarcon/warp/pkg/bash"
)

// dirStack is shellcore's bash.DirStack implementation. State is a
// stack of pseudofile-namespace directories; index 0 is the current
// working directory shown in the prompt and used to resolve bare-name
// paths in queries.
//
// On Chdir / Push, the destination is validated against shellFS — if a
// filesystem is wired in, the path must resolve to a readable
// directory. The root "/" is always accepted. Without a filesystem
// (e.g. unit tests) validation is skipped.
type dirStack struct {
	fs    FS
	stack []string
}

func newDirStack(fs FS) *dirStack {
	return &dirStack{fs: fs, stack: []string{"/"}}
}

func (d *dirStack) setFS(fs FS) { d.fs = fs }

func (d *dirStack) Cwd() string { return d.stack[0] }

func (d *dirStack) validate(path string) error {
	if d.fs == nil || path == "/" {
		return nil
	}
	if _, err := d.fs.ReadDir(path); err != nil {
		return fmt.Errorf("no such directory")
	}
	return nil
}

func (d *dirStack) Chdir(path string) error {
	if err := d.validate(path); err != nil {
		return err
	}
	d.stack[0] = path
	return nil
}

func (d *dirStack) Push(path string) error {
	if err := d.validate(path); err != nil {
		return err
	}
	d.stack = append([]string{path}, d.stack...)
	return nil
}

func (d *dirStack) Pop() error {
	if len(d.stack) < 2 {
		return fmt.Errorf("directory stack empty")
	}
	d.stack = d.stack[1:]
	return nil
}

func (d *dirStack) Stack() []string {
	out := make([]string, len(d.stack))
	copy(out, d.stack)
	return out
}

func (d *dirStack) Clear() {
	d.stack = d.stack[:1]
}

// Compile-time assertion that dirStack satisfies bash.DirStack.
var _ bash.DirStack = (*dirStack)(nil)

// NewTransientDirStack returns a fresh string-only DirStack rooted at
// "/" with no shellFS validation. Useful for non-interactive paths
// (like `weft sh -c`) that need to simulate a cd-chain rewrite without
// having a long-lived shellcore instance to draw state from.
func NewTransientDirStack() bash.DirStack {
	return newDirStack(nil)
}

// execPushdSwap rotates the top two stack entries (bash's `pushd`
// with no arguments). Returns false if the stack has fewer than 2
// entries (which has already been reported to the user).
func (c *ShellCore) execPushdSwap() bool {
	s := c.dirStack.Stack()
	if len(s) < 2 {
		c.appendScrollback(ErrorText("pushd: no other directory") + "\n")
		return false
	}
	swapped := make([]string, len(s))
	copy(swapped, s)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	c.dirStack.stack = swapped
	c.appendScrollback(formatDirstack(c.dirStack.Stack(), "") + "\n")
	return true
}

// execPushdPath pushes target onto the dirstack, after resolving it
// against cwd and validating against shellFS.
func (c *ShellCore) execPushdPath(target string) {
	resolved := c.resolvePath(target)
	if err := c.dirStack.Push(resolved); err != nil {
		c.appendScrollback(ErrorText(fmt.Sprintf("pushd: %s: %s", target, err)) + "\n")
		return
	}
	c.appendScrollback(formatDirstack(c.dirStack.Stack(), "") + "\n")
}

// execPopd pops the top of the dirstack and prints the new state.
func (c *ShellCore) execPopd() {
	if err := c.dirStack.Pop(); err != nil {
		c.appendScrollback(ErrorText(fmt.Sprintf("popd: %s", err)) + "\n")
		return
	}
	c.appendScrollback(formatDirstack(c.dirStack.Stack(), "") + "\n")
}

// execDirsFlags handles `dirs -p`, `dirs -v`, `dirs -c`. Anything else
// is reported as an invalid option, matching warp's builtinDirs.
func (c *ShellCore) execDirsFlags(arg string) {
	switch arg {
	case "-p":
		for _, d := range c.dirStack.Stack() {
			c.appendScrollback(d + "\n")
		}
	case "-v":
		for i, d := range c.dirStack.Stack() {
			c.appendScrollback(fmt.Sprintf("%2d  %s\n", i, d))
		}
	case "-c":
		c.dirStack.Clear()
	default:
		c.appendScrollback(ErrorText(fmt.Sprintf("dirs: %s: invalid option", arg)) + "\n")
	}
}

// formatDirstack joins the stack with spaces, replacing a leading
// home prefix with "~" — matching bash's `dirs` output. home is
// treated as empty by default since the weft shell doesn't have a
// per-user home concept; left configurable for tests.
func formatDirstack(stack []string, home string) string {
	parts := make([]string, len(stack))
	for i, d := range stack {
		parts[i] = abbreviateHome(d, home)
	}
	return joinSpace(parts)
}

func abbreviateHome(path, home string) string {
	if home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	if len(path) > len(home) && path[:len(home)] == home && path[len(home)] == '/' {
		return "~" + path[len(home):]
	}
	return path
}

func joinSpace(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}
