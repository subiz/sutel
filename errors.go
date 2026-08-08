package sutel

import (
	"errors"
	"strings"
)

var (
	ErrTransport          = errors.New("SIP or RTP transport failed")
	ErrProtocol           = errors.New("SIP protocol failed")
	ErrNegotiation        = errors.New("media negotiation failed")
	ErrVerification       = errors.New("call verification failed")
	ErrInvalidExpectation = errors.New("invalid test expectation")
)

// Error contains structured diagnostics for a failed Sutel operation.
type Error struct {
	Kind     error
	Scenario string
	Detail   string
	Cause    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "sutel"
	if e.Scenario != "" {
		message += " " + e.Scenario
	}
	if e.Kind != nil {
		message += ": " + e.Kind.Error()
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	if cause := causeDetail(e.Kind, e.Cause); cause != "" {
		message += ": " + cause
	}
	return message
}

func causeDetail(kind, cause error) string {
	if cause == nil {
		return ""
	}
	detail := cause.Error()
	if kind == nil || !errors.Is(cause, kind) {
		return detail
	}
	prefix := kind.Error()
	if detail == prefix {
		return ""
	}
	return strings.TrimPrefix(detail, prefix+": ")
}

func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	var result []error
	if e.Kind != nil {
		result = append(result, e.Kind)
	}
	if e.Cause != nil {
		result = append(result, e.Cause)
	}
	return result
}
