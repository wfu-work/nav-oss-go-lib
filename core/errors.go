package core

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound       = errors.New("object not found")
	ErrAccessDenied   = errors.New("access denied")
	ErrConflict       = errors.New("conflict")
	ErrInvalidConfig  = errors.New("invalid storage configuration")
	ErrInvalidKey     = errors.New("invalid object key")
	ErrInvalidRange   = errors.New("invalid byte range")
	ErrInvalidPart    = errors.New("invalid multipart part")
	ErrUnsupported    = errors.New("operation unsupported")
	ErrTemporary      = errors.New("temporary storage failure")
	ErrPartialFailure = errors.New("operation partially completed")
)

// Error adds stable provider-neutral classification to provider errors.
type Error struct {
	Op       string
	Provider Provider
	Key      string
	Code     string
	Kind     error
	Err      error
}

func (e *Error) Error() string {
	message := fmt.Sprintf("oss %s", e.Op)
	if e.Provider != "" {
		message += " provider=" + string(e.Provider)
	}
	if e.Key != "" {
		message += " key=" + e.Key
	}
	if e.Code != "" {
		message += " code=" + e.Code
	}
	if e.Err != nil {
		return message + ": " + e.Err.Error()
	}
	if e.Kind != nil {
		return message + ": " + e.Kind.Error()
	}
	return message
}

func (e *Error) Unwrap() error {
	return e.Err
}

func (e *Error) Is(target error) bool {
	return target != nil && (target == e.Kind || errors.Is(e.Err, target))
}

func newError(op string, provider Provider, key string, kind, cause error) error {
	return &Error{Op: op, Provider: provider, Key: key, Kind: kind, Err: cause}
}
