package apple

import (
	"errors"
	"fmt"
	"net/http"
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

func TestIsRateLimitedRecognizesHTTPAndHMEThrottleCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{
			name: "HTTP 429",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusTooManyRequests},
			want: true,
		},
		{
			name: "HME batch limit",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "  " + hmeRateLimitCodeBatch + "  "},
			want: true,
		},
		{
			name: "expired HME candidate is not a throttle",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41003"},
			want: false,
		},
		{
			name: "unrelated business error",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41099"},
		},
		{
			name: "rate limit in a later joined cause",
			err: errors.Join(
				&Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41099"},
				fmt.Errorf("wrapped: %w", &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: hmeRateLimitCodeBatch}),
			),
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRateLimited(test.err); got != test.want {
				t.Fatalf("IsRateLimited() = %v, want %v", got, test.want)
			}
		})
	}
}
