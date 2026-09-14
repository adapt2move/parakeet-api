package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/adapt2move/parakeet-api/internal/formats"
	"github.com/adapt2move/parakeet-api/internal/store"
)

// apiError is a client-safe failure. Its message never contains request data.
type apiError struct {
	status     int
	message    string
	retryAfter int  // seconds; 0 omits the header
	plain      bool // {"error": message} even under /v1/, as middleware rejections were
	detail     bool // FastAPI's {"detail": message}, used for unparseable JSON bodies
}

func (e *apiError) Error() string { return e.message }

func newError(status int, message string) *apiError {
	return &apiError{status: status, message: message}
}

var (
	errUnauthorized    = &apiError{status: http.StatusUnauthorized, message: "Unauthorized", plain: true}
	errRequestTooLarge = &apiError{status: http.StatusRequestEntityTooLarge, message: "Request too large", plain: true}
	errSlotsFull       = &apiError{status: http.StatusTooManyRequests, message: "Upload slots full", retryAfter: 5, plain: true}
	errRequestTimeout  = &apiError{status: http.StatusGatewayTimeout, message: "Request timed out", plain: true}

	// Raised while reading a body; they take the shape of the path like handler errors did.
	errBodyTooLarge = newError(http.StatusRequestEntityTooLarge, "Request too large")
	errStalled      = newError(http.StatusRequestTimeout, "Upload stalled")
	errTooSlow      = newError(http.StatusRequestTimeout, "Upload too slow")
	errDisconnected = newError(499, "Client disconnected")
	errBadBody      = newError(http.StatusBadRequest, "Invalid request body")

	errNotFound         = newError(http.StatusNotFound, "Not Found")
	errMethodNotAllowed = newError(http.StatusMethodNotAllowed, "Method Not Allowed")
	errInternal         = newError(http.StatusInternalServerError, "Internal Server Error")

	errInvalidFields  = newError(http.StatusUnprocessableEntity, "Invalid or unsupported request fields")
	errBodyParse      = &apiError{status: http.StatusBadRequest, message: "There was an error parsing the body", detail: true}
	errResultOrError  = newError(http.StatusBadRequest, "Provide exactly one of result or error")
	errInvalidOptions = newError(http.StatusBadRequest, "Invalid transcription options or audio")
	errMissingLease   = newError(http.StatusConflict, "Missing lease token")

	errAudioTooLarge   = newError(http.StatusRequestEntityTooLarge, "Audio too large")
	errAudioEmpty      = newError(http.StatusBadRequest, "Audio is empty")
	errTransferTimeout = newError(http.StatusRequestTimeout, "Audio transfer timed out")
)

// asAPIError maps any error to the response it produces.
func asAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var se *store.Error
	if errors.As(err, &se) {
		return &apiError{status: se.Status, message: se.Message, retryAfter: se.RetryAfter}
	}
	switch {
	case errors.Is(err, formats.ErrBodyParse):
		return errBodyParse
	case errors.Is(err, formats.ErrInvalid):
		return errInvalidFields
	case errors.Is(err, formats.ErrResultOrError):
		return errResultOrError
	}
	return errInternal
}

// response is a complete, buffered reply.
type response struct {
	status      int
	contentType string
	body        []byte
}

func jsonResponse(body []byte) response {
	return response{status: http.StatusOK, contentType: "application/json", body: body}
}

func textResponse(contentType, text string) response {
	return response{status: http.StatusOK, contentType: contentType + "; charset=utf-8", body: []byte(text)}
}

var okBody = []byte(`{"ok":true}`)

// errorBody renders the error in the shape the path uses.
func errorBody(path string, e *apiError) []byte {
	switch {
	case e.detail:
		b := append([]byte(`{"detail":`), formats.AppendString(nil, e.message)...)
		return append(b, '}')
	case !e.plain && strings.HasPrefix(path, "/v1/"):
		b := []byte(`{"error":{"message":`)
		b = formats.AppendString(b, e.message)
		b = append(b, `,"type":"invalid_request_error","code":"`...)
		b = strconv.AppendInt(b, int64(e.status), 10)
		return append(b, `"}}`...)
	default:
		b := append([]byte(`{"error":`), formats.AppendString(nil, e.message)...)
		return append(b, '}')
	}
}

func writeResponse(w http.ResponseWriter, res response) {
	h := w.Header()
	if res.contentType != "" {
		h.Set("Content-Type", res.contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(res.body)))
	w.WriteHeader(res.status)
	w.Write(res.body)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	e := asAPIError(err)
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeResponse(w, response{status: e.status, contentType: "application/json", body: errorBody(r.URL.Path, e)})
}
