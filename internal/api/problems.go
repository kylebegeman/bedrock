package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/kylebegeman/helios/internal/compat"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/state"
)

// Error preserves human messages and stable fields for automation. Older
// daemon responses still produce an Error with an empty code.
type Error struct {
	Message    string
	Code       string
	Retryable  bool
	APIVersion int
	Status     int
}

func (e *Error) Error() string { return e.Message }
func problemFor(status int, err error) compat.Problem {
	p := compat.Problem{Error: err.Error(), Code: "internal_error", APIVersion: compat.APIVersion, Status: status}
	switch {
	case errors.Is(err, state.ErrNotFound):
		p.Code = "not_found"
	case errors.Is(err, state.ErrLeased):
		p.Code = "operation_leased"
		p.Retryable = true
	case errors.Is(err, context.Canceled):
		p.Code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		p.Code = "deadline_exceeded"
		p.Retryable = true
	case errors.Is(err, kernel.ErrPlanChanged):
		p.Code = "plan_changed"
	case errors.Is(err, kernel.ErrUnknownStep):
		p.Code = "unknown_step"
	case status == http.StatusNotFound:
		p.Code = "not_found"
	case status == http.StatusBadRequest:
		p.Code = "invalid_request"
	case status == http.StatusConflict:
		p.Code = "conflict"
	case status == http.StatusServiceUnavailable:
		p.Code = "unavailable"
		p.Retryable = true
	}
	return p
}
