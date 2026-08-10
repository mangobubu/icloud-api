package hmesync

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"icloud-api/internal/apple"
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

func TestCodedErrorIsTraversesCauseOnce(t *testing.T) {
	count := 0
	target := errors.New("target")
	cause := &countingIsError{count: &count, err: target}
	err := wrapError(CodeUpstreamError, ErrUpstream, cause)

	if !errors.Is(err, target) {
		t.Fatal("errors.Is should find the wrapped target")
	}
	if count != 1 {
		t.Fatalf("wrapped Is calls = %d, want 1", count)
	}
}

func TestCodeFindsFirstCodedErrorInJoinedError(t *testing.T) {
	joined := errors.Join(
		wrapError(CodeRateLimited, ErrRateLimited, nil),
		wrapError(CodeUpstreamError, ErrUpstream, nil),
	)
	if got := Code(joined); got != CodeRateLimited {
		t.Fatalf("Code(joined) = %q, want %q", got, CodeRateLimited)
	}
}

func TestCryptoErrorKeepsStableCodeAndTypedCause(t *testing.T) {
	cause := errors.New("cipher fixture failed")
	err := wrapCryptoError(cause)
	if Code(err) != CodeCryptoError || !errors.Is(err, ErrCrypto) || !errors.Is(err, cause) {
		t.Fatalf("crypto error = %v code=%q", err, Code(err))
	}
}

func TestPendingConfirmationMarkerPreservesDiagnosticCode(t *testing.T) {
	cause := wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	err := markPendingConfirmation(cause)
	if Code(err) != CodeAccountMismatch || !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("marked error = %v code=%q", err, Code(err))
	}
	var marker interface{ PendingConfirmation() bool }
	if !errors.As(err, &marker) || !marker.PendingConfirmation() {
		t.Fatalf("pending marker missing from %T", err)
	}
}

func TestRemoteSideEffectMarkerPreservesDiagnosticCode(t *testing.T) {
	cause := wrapPersistenceError(errors.New("database fixture failed"))
	err := markRemoteSideEffectPossible(cause)
	if Code(err) != CodePersistenceError || !errors.Is(err, ErrPersistence) {
		t.Fatalf("marked error = %v code=%q", err, Code(err))
	}
	var marker interface{ RemoteSideEffectPossible() bool }
	if !errors.As(err, &marker) || !marker.RemoteSideEffectPossible() {
		t.Fatalf("remote side-effect marker missing from %T", err)
	}
	if got := markRemoteSideEffectPossible(err); got != err {
		t.Fatal("marking an already marked error changed its identity")
	}
}

func TestContextOnlyErrorDoesNotHideJoinedDiagnostic(t *testing.T) {
	if !contextOnlyError(errors.Join(context.Canceled, context.DeadlineExceeded)) {
		t.Fatal("pure joined context error was not recognized")
	}
	if contextOnlyError(errors.Join(wrapError(CodeUpstreamError, ErrUpstream, nil), context.Canceled)) {
		t.Fatal("stable upstream diagnostic was classified as context-only")
	}
}
