// Package http holds the shared HTTP plumbing: request IDs, structured access logs, panic
// recovery, body limits, the consistent error body (brief 10.9) and the health endpoints.
package http

import (
	nethttp "net/http"

	"github.com/gin-gonic/gin"
)

// FieldError describes one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ErrorBody is the single error shape returned by every failing request.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Fields    []FieldError `json:"fields,omitempty"`
	RequestID string       `json:"request_id"`
}

// WriteError aborts the request with the standard error body.
func WriteError(c *gin.Context, status int, code, message string, fields ...FieldError) {
	c.AbortWithStatusJSON(status, ErrorBody{Error: ErrorDetail{
		Code: code, Message: message, Fields: fields, RequestID: RequestIDFrom(c),
	}})
}

func notFound(c *gin.Context) {
	WriteError(c, nethttp.StatusNotFound, "not_found", "The requested resource does not exist.")
}

func methodNotAllowed(c *gin.Context) {
	WriteError(c, nethttp.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed for the resource.")
}
