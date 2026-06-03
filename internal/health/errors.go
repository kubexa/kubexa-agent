package health

import "errors"

// checkerError annotates a health check failure with a component status label.
type checkerError struct {
	status string
	cause  error
}

func (e *checkerError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.status
}

func (e *checkerError) Unwrap() error {
	return e.cause
}

// Unhealthy wraps err as an unhealthy component check failure.
func Unhealthy(err error) error {
	if err == nil {
		return nil
	}
	return &checkerError{status: "unhealthy", cause: err}
}

// Degraded wraps err as a degraded component check failure.
func Degraded(err error) error {
	if err == nil {
		return nil
	}
	return &checkerError{status: "degraded", cause: err}
}

func componentStatus(err error) (status string, msg *string) {
	if err == nil {
		return "ok", nil
	}
	text := err.Error()
	var ce *checkerError
	if errors.As(err, &ce) {
		status = ce.status
	} else {
		status = "unhealthy"
	}
	return status, &text
}
