package oversight

import (
	"log"
	"time"
)

// Option are applied to change the behavior of a Tree.
type Option func(*Tree)

// WithSpecification defines a custom setup to tweak restart tolerance and
// strategy for the instance of oversight.
func WithSpecification(maxR int, maxT time.Duration, strategy Strategy) Option {
	return func(t *Tree) {
		WithRestartIntensity(maxR, maxT)(t)
		WithRestartStrategy(strategy)(t)
	}
}

// WithRestartIntensity defines a custom tolerance for failures in the
// supervisor tree.
func WithRestartIntensity(maxR int, maxT time.Duration) Option {
	return func(t *Tree) {
		t.maxR, t.maxT = maxR, maxT
	}
}

// Default restart intensity expectations.
const (
	DefaultMaxR = 1
	DefaultMaxT = 5 * time.Second
)

// DefaultRestartIntensity redefines the tolerance for failures in the
// supervisor tree. It defaults to 1 restart (maxR) in the preceding 5 seconds
// (maxT).
func DefaultRestartIntensity(t *Tree) {
	t.maxR, t.maxT = DefaultMaxR, DefaultMaxT
}

// WithRestartStrategy defines a custom restart strategy for the supervisor
// tree.
func WithRestartStrategy(strategy Strategy) Option {
	return func(t *Tree) {
		t.strategy = strategy
	}
}

// DefaultRestartStrategy redefines the supervisor behavior to use OneForOne.
func DefaultRestartStrategy(t *Tree) {
	t.strategy = OneForOne
}

// Processes plugs one or more Permanent child processes to the supervisor tree.
// Processes never reset the child process list.
func Processes(processes ...ChildProcess) Option {
	return func(t *Tree) {
		for _, p := range processes {
			Process(Permanent, p)(t)
		}
	}
}

// Process plugs one child processes to the supervisor tree. Process never reset
// the child process list.
func Process(restart Restart, process ChildProcess) Option {
	return func(t *Tree) {
		t.processes = append(t.processes, childProcess{
			restart: restart,
			f:       process,
		})
		t.states = append(t.states, state{})
	}
}

// WithLogger plugs a custom logger to the oversight tree.
func WithLogger(logger *log.Logger) Option {
	return func(t *Tree) {
		t.logger = logger
	}
}
