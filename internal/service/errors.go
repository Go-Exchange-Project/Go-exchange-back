package service

import (
	"errors"
	"fmt"
)

type ErrorKind string

const (
	ErrorKindValidation ErrorKind = "VALIDATION"
	ErrorKindConflict   ErrorKind = "CONFLICT"
	ErrorKindForbidden  ErrorKind = "FORBIDDEN"
)

type DomainError struct {
	Kind    ErrorKind
	Message string
}

func (e *DomainError) Error() string {
	return e.Message
}

func NewValidationErrorf(format string, args ...interface{}) error {
	return &DomainError{Kind: ErrorKindValidation, Message: fmt.Sprintf(format, args...)}
}

func NewConflictErrorf(format string, args ...interface{}) error {
	return &DomainError{Kind: ErrorKindConflict, Message: fmt.Sprintf(format, args...)}
}

func NewForbiddenErrorf(format string, args ...interface{}) error {
	return &DomainError{Kind: ErrorKindForbidden, Message: fmt.Sprintf(format, args...)}
}

func DomainErrorKind(err error) (ErrorKind, bool) {
	var domainErr *DomainError
	if errors.As(err, &domainErr) {
		return domainErr.Kind, true
	}
	return "", false
}
