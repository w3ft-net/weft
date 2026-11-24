// Package testhost builds a shell.Shell over a Host made of plain callbacks,
// for tests that drive a core directly.
package testhost

import shellcore "github.com/graybeardcorp/warp/pkg/shell"

type host struct {
	w, h     int
	write    func([]byte)
	dispatch func(shellcore.Action)
}

func (h host) Write(b []byte)              { h.write(b) }
func (h host) Dispatch(a shellcore.Action) { h.dispatch(a) }
func (h host) Size() (int, int)            { return h.w, h.h }

// New returns a core of the given size whose output goes to write and whose
// Actions go to dispatch.
func New(width, height int, write func([]byte), dispatch func(shellcore.Action)) *shellcore.Shell {
	return shellcore.New(host{width, height, write, dispatch})
}

type asyncHost struct {
	host
	post func(func())
}

func (a asyncHost) Post(fn func()) { a.post(fn) }

// NewAsync is New over a Host that also implements shell.Poster, so each line
// runs on its own goroutine and comes back through post.
func NewAsync(width, height int, write func([]byte), dispatch func(shellcore.Action), post func(func())) *shellcore.Shell {
	return shellcore.New(asyncHost{host{width, height, write, dispatch}, post})
}
