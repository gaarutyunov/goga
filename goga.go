// Package goga holds the two declarations every goga module needs and nothing
// else: the generic [Option] type and [Apply], the fold that turns a module's
// defaults plus a caller's options into its settings value.
//
// The package is deliberately a leaf. It imports nothing but the standard
// library, and it must stay that way: every module in goga imports it, so any
// dependency added here is a dependency forced on all of them. The composition
// root goga/app sits at the other end — it imports every module — so goga and
// goga/app can never be the same package without making the import graph
// cyclic.
//
// The functional-options pattern here is one step past the usual one. An
// option is parameterised over the settings type S it mutates, so a module can
// keep S unexported and still hand out options for it: callers name the option
// constructors, never the struct. [Apply] is then the only way an S is ever
// produced from defaults, which is what makes that possible.
//
//	type settings struct{ addr string }
//
//	func WithAddr(a string) goga.Option[settings] {
//		return func(s *settings) error {
//			if a == "" {
//				return errors.New("addr must not be empty")
//			}
//			s.addr = a
//			return nil
//		}
//	}
//
// Returning an error lets an option validate its own input, so a bad value
// fails at the call site that supplied it rather than at first use somewhere
// deeper in the program.
package goga

import "fmt"

// Option mutates a module's private settings. Every exported goga constructor
// takes ...Option[S] for its own unexported settings type S. Returning an error
// lets an option validate its own input, so a bad value fails at the call site
// rather than at first use.
type Option[S any] func(*S) error

// Apply folds options over a module's defaults. It is the only way an S is ever
// produced, which is what keeps S unexported.
//
// Options are applied in order, so a later option overrides an earlier one. The
// first option to return an error stops the fold; the partially applied value is
// returned alongside the wrapped error and must not be used.
func Apply[S any](defaults S, opts ...Option[S]) (S, error) {
	for _, opt := range opts {
		if err := opt(&defaults); err != nil {
			return defaults, fmt.Errorf("goga: applying option: %w", err)
		}
	}
	return defaults, nil
}
