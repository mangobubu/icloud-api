package apple

import (
	"errors"
	"testing"
)

type countingIsError struct {
	count *int
	err   error
}

func (e *countingIsError) Error() string { return "counting error" }

func (e *countingIsError) Is(target error) bool {
	(*e.count)++
	return target == e.err
}

func (e *countingIsError) Unwrap() error { return e.err }

func TestErrorIsTraversesCauseOnce(t *testing.T) {
	count := 0
	target := errors.New("target")
	cause := &countingIsError{count: &count, err: target}
	err := &Error{Kind: ErrService, Err: cause}

	if !errors.Is(err, target) {
		t.Fatal("errors.Is should find the wrapped target")
	}
	if count != 1 {
		t.Fatalf("wrapped Is calls = %d, want 1", count)
	}
}
