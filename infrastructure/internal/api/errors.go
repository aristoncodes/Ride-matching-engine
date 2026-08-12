package api

import (
	"encoding/json"
	"net/http"
)

// APIError is the ONE shape every non-2xx response takes.
//
// A single, documented error envelope is not tidiness for its own sake: a B2B
// client writes its error handling once, against this. An API that returns a
// JSON object here, a bare string there, and Go's default `http.Error` text
// somewhere else forces every integrator to write a parser per endpoint, and
// they will get it wrong.
//
//	{
//	  "error": {
//	    "code": "invalid_argument",
//	    "message": "pickup.lat must be between -90 and 90",
//	    "field": "pickup.lat",
//	    "request_id": "req_01H..."
//	  }
//	}
type APIError struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the machine-readable code and the human-readable message.
type ErrorBody struct {
	// Code is stable and machine-readable. Clients branch on THIS, never on the
	// message — messages get reworded, and a client matching on prose breaks
	// silently the first time someone fixes a typo.
	Code string `json:"code"`

	// Message is for a human reading a log. Written to be actionable: it says
	// what was wrong and what was expected, not just "invalid input".
	Message string `json:"message"`

	// Field names the offending input when there is exactly one, so a client
	// can highlight it without parsing the message.
	Field string `json:"field,omitempty"`

	// RequestID is the tracing key, echoed in the X-Request-ID header too.
	// This is the single most useful field in a support conversation: it turns
	// "your API broke" into one grep.
	RequestID string `json:"request_id"`
}

// Stable error codes. Additive only — removing or renaming one is a breaking
// change for every client that branches on it.
const (
	CodeInvalidArgument  = "invalid_argument"
	CodeMalformedJSON    = "malformed_json"
	CodePayloadTooLarge  = "payload_too_large"
	CodeUnauthorized     = "unauthorized"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeUnavailable      = "unavailable"
	CodeInternal         = "internal"
)

// writeError sends a structured error response.
//
// Deliberately never leaks an internal error string to the client: a Redis
// address, a stack frame, or a SQL fragment in an error body is an information
// disclosure, and it is useless to the caller anyway. The detail goes to the
// server log, and the client gets the request id needed to find it.
func writeError(w http.ResponseWriter, status int, code, message, field, requestID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Echoed as a header as well as in the body, so it is available even on a
	// response a client failed to parse.
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(APIError{Error: ErrorBody{
		Code:      code,
		Message:   message,
		Field:     field,
		RequestID: requestID,
	}})
}

// writeJSON sends a successful response.
func writeJSON(w http.ResponseWriter, status int, requestID string, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
