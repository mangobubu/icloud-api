package apple

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	// Apple's Hide My Email service reports the batch creation throttle inside
	// an HTTP 200 response. It is not expressed as HTTP 429.
	hmeRateLimitCodeBatch = "-41015"
)

var (
	ErrInvalidConfig    = errors.New("invalid Apple client configuration")
	ErrInvalidSession   = errors.New("invalid or expired Apple session")
	ErrAuthentication   = errors.New("Apple authentication failed")
	ErrTwoFactorCode    = errors.New("invalid Apple two-factor code")
	ErrTermsRequired    = errors.New("Apple account action required")
	ErrResponseTooLarge = errors.New("Apple response exceeds configured limit")
	ErrInvalidResponse  = errors.New("invalid Apple response")
	ErrService          = errors.New("Apple service returned an error")
)

// Error is the typed error returned for transport, HTTP, protocol and service
// failures. Response bodies are deliberately excluded because they can carry
// authentication material.
type Error struct {
	Op          string
	Kind        error
	StatusCode  int
	ServiceCode string
	Retryable   bool
	Err         error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "Apple request failed"
	if e.Op != "" {
		message = "Apple " + e.Op + " failed"
	}
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.ServiceCode != "" {
		message += " (service " + e.ServiceCode + ")"
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Err != nil {
		return e.Err
	}
	return e.Kind
}

func (e *Error) Is(target error) bool {
	// Keep this comparison shallow. The errors package follows Unwrap after
	// calling Is, so walking Err here would duplicate traversal of the chain.
	return e != nil && target == e.Kind
}

func operationError(op string, kind error, status int, cause error) error {
	return &Error{
		Op:         op,
		Kind:       kind,
		StatusCode: status,
		Retryable:  retryableStatus(status) || (status == 0 && kind == ErrService && cause != nil),
		Err:        cause,
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// IsRateLimited reports both HTTP-level throttling and Hide My Email business
// throttles returned in a successful HTTP response envelope.
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	if upstream, ok := err.(*Error); ok && isRateLimitedError(upstream) {
		return true
	}
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range unwrapped.Unwrap() {
			if IsRateLimited(child) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return IsRateLimited(unwrapped.Unwrap())
	}
	return false
}

func isRateLimitedError(upstream *Error) bool {
	if upstream == nil {
		return false
	}
	if upstream.StatusCode == http.StatusTooManyRequests {
		return true
	}
	switch strings.TrimSpace(upstream.ServiceCode) {
	case hmeRateLimitCodeBatch:
		return true
	default:
		return false
	}
}
