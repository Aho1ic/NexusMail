package ports

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifiedErrorsCarryTheirKind(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		kind  error
		other error
	}{
		{"invalid", Invalidf("account_id is required"), ErrInvalidInput, ErrConflict},
		{"conflict", Conflictf("revision conflict"), ErrConflict, ErrNotFound},
		{"not found", NotFoundf("draft 7"), ErrNotFound, ErrUnavailable},
		{"unavailable", Unavailablef("account is offline"), ErrUnavailable, ErrInvalidInput},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if !errors.Is(test.err, test.kind) {
				t.Errorf("errors.Is(%v, its own kind) = false", test.err)
			}
			if errors.Is(test.err, test.other) {
				t.Errorf("%v also matched an unrelated kind", test.err)
			}
		})
	}
}

func TestClassifiedErrorKeepsMessageAndCause(t *testing.T) {
	cause := errors.New("underlying driver failure")
	err := Invalidf("bad cursor: %w", cause)

	if got, want := err.Error(), "bad cursor: underlying driver failure"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Error("classification was lost")
	}
	if !errors.Is(err, cause) {
		t.Error("wrapped cause is no longer reachable")
	}
}

// A classified error must survive being wrapped again by a caller, which is what
// happens whenever a service annotates a repository failure.
func TestClassifiedErrorSurvivesFurtherWrapping(t *testing.T) {
	err := fmt.Errorf("update draft: %w", Conflictf("revision conflict"))
	if !errors.Is(err, ErrConflict) {
		t.Fatal("classification did not survive an outer wrap")
	}
	if errors.Is(err, ErrInvalidInput) {
		t.Fatal("wrapped error matched the wrong kind")
	}
}
