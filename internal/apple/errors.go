package apple

import (
	"errors"
	"fmt"
	"net/http"
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
	return e != nil && (target == e.Kind || errors.Is(e.Err, target))
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
