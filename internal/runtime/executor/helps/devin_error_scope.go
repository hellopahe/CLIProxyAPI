package helps

import (
	"errors"
	"net/http"
	"strings"
)

type devinRequestScopedError struct{ error }

func (e devinRequestScopedError) Unwrap() error         { return e.error }
func (e devinRequestScopedError) IsRequestScoped() bool { return true }
func (e devinRequestScopedError) StatusCode() int       { return http.StatusForbidden }

// ScopeDevinRequestError preserves a content-policy refusal and its HTTP status,
// but prevents a request-specific refusal from cooling unrelated requests for
// the same credential/model. It does not retry or modify the rejected prompt.
func ScopeDevinRequestError(err error) error {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() == http.StatusForbidden && strings.Contains(strings.ToLower(err.Error()), "your request was blocked by our content policy") {
		return devinRequestScopedError{err}
	}
	return err
}
