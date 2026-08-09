package hmesync

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"icloud-api/internal/apple"
)

func TestMapAppleErrorKeepsStableCodesAndTypedCause(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		duringVerification bool
		wantCode           string
		wantKind           error
	}{
		{
			name:     "expired session",
			err:      apple.ErrInvalidSession,
			wantCode: CodeSessionExpired,
			wantKind: ErrSessionExpired,
		},
		{
			name:     "rate limited",
			err:      &apple.Error{Kind: apple.ErrService, StatusCode: http.StatusTooManyRequests, Retryable: true},
			wantCode: CodeRateLimited,
			wantKind: ErrRateLimited,
		},
		{
			name:     "temporary upstream failure",
			err:      &apple.Error{Kind: apple.ErrService, StatusCode: http.StatusServiceUnavailable, Retryable: true},
			wantCode: CodeUpstreamError,
			wantKind: ErrUpstream,
		},
		{
			name:     "account action required",
			err:      &apple.Error{Kind: apple.ErrTermsRequired, StatusCode: http.StatusPreconditionFailed},
			wantCode: CodeAccountActionRequired,
			wantKind: ErrAccountActionRequired,
		},
		{
			name:               "verification authentication failure",
			err:                apple.ErrAuthentication,
			duringVerification: true,
			wantCode:           CodeVerificationInvalid,
			wantKind:           ErrVerificationInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mapAppleError(test.err, test.duringVerification)
			if Code(got) != test.wantCode || got.Error() != test.wantCode || !errors.Is(got, test.wantKind) {
				t.Fatalf("mapped error = %v code=%q, want %s / %v", got, Code(got), test.wantCode, test.wantKind)
			}
			var inputAppleError *apple.Error
			if errors.As(test.err, &inputAppleError) {
				var mappedAppleError *apple.Error
				if !errors.As(got, &mappedAppleError) || mappedAppleError != inputAppleError {
					t.Fatalf("typed Apple cause was not preserved: got %#v want %#v", mappedAppleError, inputAppleError)
				}
			}
		})
	}
}

func TestMapAppleErrorPreservesContextCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := mapAppleError(err, false); got != err || Code(got) != "" {
			t.Fatalf("context error = %v code=%q, want original error", got, Code(got))
		}
	}
}
