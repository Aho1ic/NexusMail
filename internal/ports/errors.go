package ports

import (
	"errors"
	"fmt"
)

// The sentinels below are the vocabulary the transport layer uses to turn a
// failure into an HTTP status. A service or provider states the class of its own
// failure by returning one of the constructors underneath; nothing upstream
// inspects error text.
//
// The transport used to classify by substring — an error containing "invalid" or
// "required" became 400, "offline" became 503, and only the 500 branch redacted
// the message. Any internal failure whose text happened to contain one of those
// words was therefore reported verbatim to the client, and any intentional
// 400 whose wording drifted silently became a redacted 500. Both directions were
// invisible in review because the coupling lived in a string.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalidInput = errors.New("invalid input")
	ErrUnavailable  = errors.New("unavailable")
)

// classified pairs a caller-facing message with the sentinel that decides its
// status. The message is exactly what the constructor was given, so classifying
// an error never changes what the client reads.
type classified struct {
	kind  error
	cause error
}

// Error returns the formatted message, not the sentinel's name.
func (e *classified) Error() string { return e.cause.Error() }

// Is answers for the sentinel this error was classified as. Unwrap keeps the
// original cause reachable, so errors.Is against a lower-level sentinel (a
// driver's or the provider library's) still works through a classification.
func (e *classified) Is(target error) bool { return target == e.kind }
func (e *classified) Unwrap() error        { return e.cause }

// classify renders the message with fmt.Errorf, so %w behaves as usual and the
// wrapped cause stays in the chain.
func classify(kind error, format string, args ...any) error {
	return &classified{kind: kind, cause: fmt.Errorf(format, args...)}
}

// Invalidf marks a failure caused by the caller's input: 400.
func Invalidf(format string, args ...any) error { return classify(ErrInvalidInput, format, args...) }

// Conflictf marks a failure caused by the resource's current state, including a
// lost optimistic-lock race: 409.
func Conflictf(format string, args ...any) error { return classify(ErrConflict, format, args...) }

// NotFoundf marks a resource that does not exist: 404.
func NotFoundf(format string, args ...any) error { return classify(ErrNotFound, format, args...) }

// Unavailablef marks a dependency that is temporarily unusable — an offline
// account, a mailbox the provider will not create: 503.
func Unavailablef(format string, args ...any) error { return classify(ErrUnavailable, format, args...) }
