// Package openai provides OpenAI-wire-format helpers: the error envelope,
// and inspection/mutation of chat completion request bodies represented as
// jsonutil.OrderedMap.
package openai

import "encoding/json"

// ErrorType is OpenAI's error.type vocabulary, extended with the values we
// need for our own synthesized errors.
type ErrorType string

const (
	ErrTypeInvalidRequest ErrorType = "invalid_request_error"
	ErrTypeAuthentication ErrorType = "authentication_error"
	ErrTypePermission     ErrorType = "permission_error"
	ErrTypeNotFound       ErrorType = "not_found_error"
	ErrTypeRateLimit      ErrorType = "rate_limit_error"
	ErrTypeAPIError       ErrorType = "api_error"
	ErrTypeOverloaded     ErrorType = "overloaded_error"
	ErrTypeTimeout        ErrorType = "timeout_error"
)

// ErrorBody is the OpenAI-compatible error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string      `json:"message"`
	Type    ErrorType   `json:"type"`
	Param   interface{} `json:"param"`
	Code    interface{} `json:"code"`
}

// NewError builds a client-facing error envelope. param/code are typically
// nil; pass a string when OpenAI's schema calls for one.
func NewError(message string, typ ErrorType, param, code interface{}) ErrorBody {
	return ErrorBody{Error: ErrorDetail{Message: message, Type: typ, Param: param, Code: code}}
}

func (e ErrorBody) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}

// Model is the OpenAI /v1/models list entry schema.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
