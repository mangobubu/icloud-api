package hmesync

import (
	"context"
	"errors"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

const (
	CodeLoginRequired            = "APPLE_LOGIN_REQUIRED"
	CodeSessionExpired           = "APPLE_SESSION_EXPIRED"
	CodeCredentialsInvalid       = "APPLE_CREDENTIALS_INVALID"
	CodeVerificationInvalid      = "APPLE_VERIFICATION_INVALID"
	CodeFlowExpired              = "APPLE_FLOW_EXPIRED"
	CodeAccountActionRequired    = "APPLE_ACCOUNT_ACTION_REQUIRED"
	CodeRateLimited              = "APPLE_RATE_LIMITED"
	CodeUpstreamError            = "APPLE_UPSTREAM_ERROR"
	CodeAliasConfirmationPending = domain.AppleAliasConfirmationPending
	CodeAccountMismatch          = "APPLE_ACCOUNT_MISMATCH"
	CodeAccountChanged           = "ACCOUNT_CHANGED"
	CodeAliasOwnershipConflict   = "ALIAS_OWNERSHIP_CONFLICT"
)

var (
	ErrLoginRequired            = errors.New("Apple login required")
	ErrSessionExpired           = errors.New("Apple session expired")
	ErrCredentialsInvalid       = errors.New("Apple credentials invalid")
	ErrVerificationInvalid      = errors.New("Apple verification code invalid")
	ErrFlowExpired              = errors.New("Apple verification flow expired")
	ErrAccountActionRequired    = errors.New("Apple account action required")
	ErrRateLimited              = errors.New("Apple request rate limited")
	ErrUpstream                 = errors.New("Apple upstream request failed")
	ErrAliasConfirmationPending = errors.New("Apple alias confirmation pending")
	ErrAccountMismatch          = errors.New("Apple account does not own this mailbox")
	ErrAccountChanged           = errors.New("account identity changed during Apple operation")
	ErrAliasOwnershipConflict   = errors.New("alias belongs to another account")
)

type codedError struct {
	code  string
	kind  error
	cause error
}

func (e *codedError) Error() string { return e.code }

func (e *codedError) Unwrap() error { return e.cause }

func (e *codedError) Is(target error) bool {
	return target == e.kind || errors.Is(e.cause, target)
}

func wrapError(code string, kind, cause error) error {
	return &codedError{code: code, kind: kind, cause: cause}
}

// Code returns the stable API error code for a typed synchronization error.
func Code(err error) string {
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return ""
}

func mapAppleError(err error, duringVerification bool) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, apple.ErrInvalidSession):
		return wrapError(CodeSessionExpired, ErrSessionExpired, err)
	case errors.Is(err, apple.ErrAuthentication) && duringVerification:
		return wrapError(CodeVerificationInvalid, ErrVerificationInvalid, err)
	case errors.Is(err, apple.ErrAuthentication):
		return wrapError(CodeCredentialsInvalid, ErrCredentialsInvalid, err)
	case errors.Is(err, apple.ErrTwoFactorCode):
		return wrapError(CodeVerificationInvalid, ErrVerificationInvalid, err)
	case errors.Is(err, apple.ErrTermsRequired):
		return wrapError(CodeAccountActionRequired, ErrAccountActionRequired, err)
	}
	var upstream *apple.Error
	if errors.As(err, &upstream) && upstream.StatusCode == 429 {
		return wrapError(CodeRateLimited, ErrRateLimited, err)
	}
	return wrapError(CodeUpstreamError, ErrUpstream, err)
}
