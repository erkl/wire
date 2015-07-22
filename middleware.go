package wire

import (
	"github.com/erkl/heat"
)

var _ RoundTripper = new(Transport)

// Objects implementing the RoundTripper interface are capable of issuing
// HTTP requests and returning the responses.
type RoundTripper interface {
	RoundTrip(req *heat.Request) (*heat.Response, error)
}

// A Middleware function extends a RoundTripper with additional functionality.
type Middleware func(req *heat.Request, next RoundTripper) (*heat.Response, error)

// Wrap extends a RoundTripper with one or more pieces of middleware.
//
//   // These two lines are functionally equivalent.
//   Wrap(rt, foo, bar)
//   Wrap(Wrap(rt, bar), foo)
func Wrap(rt RoundTripper, m ...Middleware) RoundTripper {
	for i := len(m) - 1; i >= 0; i-- {
		rt = &wrapped{m[i], rt}
	}
	return rt
}

type wrapped struct {
	fn Middleware
	rt RoundTripper
}

func (w *wrapped) RoundTrip(req *heat.Request) (*heat.Response, error) {
	return w.fn(req, w.rt)
}
