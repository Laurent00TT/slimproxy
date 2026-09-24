package executor

import "net/http"

// A missing compact endpoint does not establish that normal Responses is
// unavailable. Model-wide cooldown would turn a capability failure into a
// twelve-hour inference outage for the same otherwise usable credential.
type slimproxyCodexCompactNotFoundError struct{ statusErr }

func (slimproxyCodexCompactNotFoundError) IsRequestScoped() bool { return true }

func slimproxyCodexCompactStatusErr(statusCode int, body []byte) error {
	err := newCodexStatusErr(statusCode, body)
	if err.StatusCode() == http.StatusNotFound {
		return slimproxyCodexCompactNotFoundError{err}
	}
	return err
}
