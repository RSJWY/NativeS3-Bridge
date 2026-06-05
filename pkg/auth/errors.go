package auth

import (
	"errors"
	"fmt"
)

type Error struct {
	Code string
}

func (e Error) Error() string {
	return fmt.Sprintf("auth failed: %s", e.Code)
}

func NewError(code string) error {
	return Error{Code: code}
}

func ErrorCode(err error) string {
	var e Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "AccessDenied"
}

const (
	CodeAccessDenied          = "AccessDenied"
	CodeInvalidAccessKeyID    = "InvalidAccessKeyId"
	CodeSignatureDoesNotMatch = "SignatureDoesNotMatch"
	CodeRequestTimeTooSkewed  = "RequestTimeTooSkewed"
)
