package link

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusError is a non-success HTTP answer from the controller: a refused
// WebSocket upgrade, or a failed join or announce.
type StatusError struct {
	Status     int
	Code       string // `error` field of the body, when JSON
	Message    string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

// ReadStatusError describes a non-success response. It understands the
// controller's error bodies ({error, message}, validation {errors: [...]},
// retryAfterSeconds) and the Retry-After header, and reads at most 4 KiB.
func ReadStatusError(resp *http.Response) *StatusError {
	e := &StatusError{Status: resp.StatusCode, RetryAfter: RetryAfter(resp)}
	if resp.Body == nil {
		return e
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Error             string `json:"error"`
		Message           string `json:"message"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
		Errors            []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		e.Code, e.Message = parsed.Error, parsed.Message
		if e.Message == "" && len(parsed.Errors) > 0 {
			var parts []string
			for _, pe := range parsed.Errors {
				parts = append(parts, strings.TrimSpace(pe.Field+" "+pe.Message))
			}
			e.Message = strings.Join(parts, "; ")
		}
		if e.RetryAfter == 0 && parsed.RetryAfterSeconds > 0 {
			e.RetryAfter = time.Duration(parsed.RetryAfterSeconds) * time.Second
		}
	} else if s := strings.TrimSpace(string(body)); s != "" && len(s) < 200 {
		e.Message = s
	}
	return e
}

// RetryAfter reads a Retry-After header in seconds; 0 when absent.
func RetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}
