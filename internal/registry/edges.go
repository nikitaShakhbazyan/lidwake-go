package registry

import "sync"

// edgeStream carries the flips of one derived boolean (blocking, wants-display) from the
// registry to a single consumer, in order and without loss, without ever making a mutation wait
// for that consumer.
//
// Why a queue and not a buffered channel or a callback: the daemon applies each edge serially
// and may spend seconds on one (a helper round-trip, a pre-sleep chime). A bounded channel
// would eventually block acquire/release behind that consumer — or force dropping edges, which
// could leave the helper holding a block nobody wants. A callback would run consumer code on
// the mutating goroutine. So mutations append to an unbounded slice under a mutex that only
// guards the slice (O(1), never held while consumer code runs), and one goroutine drains the
// slice into an unbuffered channel. Edges pushed before anyone reads are kept, as with a stream
// that buffers until iteration begins. The queue only grows while the consumer is behind, and
// only by one entry per flip.
type edgeStream struct {
	mu     sync.Mutex
	queue  []bool
	closed bool

	wake  chan struct{} // capacity 1: "the queue may be non-empty"
	out   chan bool
	done  chan struct{}
	start sync.Once
}

func newEdgeStream() *edgeStream {
	return &edgeStream{
		wake: make(chan struct{}, 1),
		out:  make(chan bool),
		done: make(chan struct{}),
	}
}

// push enqueues an edge. Never blocks on the consumer.
func (s *edgeStream) push(v bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, v)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// channel starts the drain goroutine on first use and returns the delivery channel. Starting
// lazily keeps short-lived users of the registry (the CLI, tests) free of background goroutines.
func (s *edgeStream) channel() <-chan bool {
	s.start.Do(func() { go s.pump() })
	return s.out
}

// close stops delivery: undelivered edges are dropped and the channel is closed.
func (s *edgeStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.queue = nil
	s.mu.Unlock()
	close(s.done)
	// Never started: nobody will close out, so do it here; a later channel() call returns the
	// closed channel because the Once has fired.
	s.start.Do(func() { close(s.out) })
}

func (s *edgeStream) pump() {
	defer close(s.out)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.done:
				return
			}
			s.mu.Lock()
		}
		v := s.queue[0]
		s.queue = s.queue[1:]
		if len(s.queue) == 0 {
			s.queue = nil // let the backing array go once the consumer catches up
		}
		s.mu.Unlock()
		select {
		case s.out <- v:
		case <-s.done:
			return
		}
	}
}
