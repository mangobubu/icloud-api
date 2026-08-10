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
	CodeAccountDisabled          = "ACCOUNT_DISABLED"
	CodePersistenceError         = "AUTO_CREATION_PERSISTENCE_ERROR"
	CodeCryptoError              = "AUTO_CREATION_CRYPTO_ERROR"
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
	ErrAccountDisabled          = errors.New("account is disabled")
	ErrPersistence              = errors.New("automatic alias creation persistence failed")
	ErrCrypto                   = errors.New("automatic alias creation cryptographic operation failed")
)

type codedError struct {
	code  string
	kind  error
	cause error
}

func (e *codedError) Error() string { return e.code }

// DiagnosticCode exposes the stable, non-sensitive code to infrastructure
// such as the automatic-creation scheduler.  It deliberately does not expose
// the wrapped cause, which may contain request data or an upstream response.
func (e *codedError) DiagnosticCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *codedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *codedError) Is(target error) bool {
	// errors.Is traverses Unwrap (including joined errors) after consulting
	// this method. Recursing into the cause here would visit the same chain a
	// second time and can multiply work for nested errors.
	return e != nil && target == e.kind
}

func wrapError(code string, kind, cause error) error {
	return &codedError{code: code, kind: kind, cause: cause}
}

// wrapPersistenceError gives local confirmation writes a stable diagnostic
// code while preserving typed causes such as store.ErrAliasLimit. Explicit
// coded errors already carry the most useful classification and must remain
// the outer error.
func wrapPersistenceError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if Code(err) != "" {
		return err
	}
	return wrapError(CodePersistenceError, ErrPersistence, err)
}

func wrapCryptoError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if Code(err) != "" {
		return err
	}
	return wrapError(CodeCryptoError, ErrCrypto, err)
}

type pendingConfirmationError struct {
	cause error
}

func (e *pendingConfirmationError) Error() string {
	if e == nil || e.cause == nil {
		return CodeAliasConfirmationPending
	}
	return e.cause.Error()
}

func (e *pendingConfirmationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *pendingConfirmationError) PendingConfirmation() bool { return e != nil }

func markPendingConfirmation(err error) error {
	if err == nil {
		return nil
	}
	var marked interface{ PendingConfirmation() bool }
	if errors.As(err, &marked) && marked.PendingConfirmation() {
		return err
	}
	return &pendingConfirmationError{cause: err}
}

// remoteSideEffectError marks a failure that happened after Apple returned a
// reserved address but before a durable local candidate was stored. Callers
// can use the marker to pause automatic creation and avoid issuing another
// reserve for the same account.
type remoteSideEffectError struct {
	cause error
}

func (e *remoteSideEffectError) Error() string {
	if e == nil || e.cause == nil {
		return "remote alias side effect may be untracked"
	}
	return e.cause.Error()
}

func (e *remoteSideEffectError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *remoteSideEffectError) RemoteSideEffectPossible() bool { return e != nil }

func markRemoteSideEffectPossible(err error) error {
	if err == nil {
		return nil
	}
	var marked interface{ RemoteSideEffectPossible() bool }
	if errors.As(err, &marked) && marked.RemoteSideEffectPossible() {
		return err
	}
	return &remoteSideEffectError{cause: err}
}

func contextOnlyError(err error) bool {
	if err == nil {
		return false
	}
	if coded, ok := err.(*codedError); ok {
		return coded.code == "CONTEXT_CANCELED" || coded.code == "CONTEXT_DEADLINE_EXCEEDED"
	}
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := unwrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !contextOnlyError(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if child := unwrapped.Unwrap(); child != nil {
			return contextOnlyError(child)
		}
	}
	return err == context.Canceled || err == context.DeadlineExceeded
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
