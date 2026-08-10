package hmesync

import (
	"errors"
	"net/http"
	"testing"

	"icloud-api/internal/apple"
)

func TestShouldRetryAutoCreateConfirmationInspectsEveryJoinedAppleError(t *testing.T) {
	nonRetryable := &apple.Error{
		Kind:       apple.ErrService,
		StatusCode: http.StatusBadRequest,
		Retryable:  false,
	}
	retryable := &apple.Error{
		Kind:       apple.ErrService,
		StatusCode: http.StatusServiceUnavailable,
		Retryable:  true,
	}
	for name, err := range map[string]error{
		"retryable branch after non-retryable":  errors.Join(nonRetryable, retryable),
		"retryable branch before non-retryable": errors.Join(retryable, nonRetryable),
	} {
		t.Run(name, func(t *testing.T) {
			if !shouldRetryAutoCreateConfirmation(err) {
				t.Fatalf("shouldRetryAutoCreateConfirmation(%v) = false, want true", err)
			}
		})
	}
}

func TestShouldRetryAutoCreateConfirmationHonorsTypedNonRetryableAppleError(t *testing.T) {
	err := &apple.Error{
		Kind:       apple.ErrService,
		StatusCode: http.StatusBadRequest,
		Retryable:  false,
	}
	if shouldRetryAutoCreateConfirmation(err) {
		t.Fatalf("shouldRetryAutoCreateConfirmation(%v) = true, want false", err)
	}
}
