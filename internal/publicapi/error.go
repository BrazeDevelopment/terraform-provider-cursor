package publicapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	maxErrorMessageBytes = 2048
	maxRawErrorBytes     = 512
)

// Error is a Cursor public API failure. The message includes the machine-readable code and remediation text.
type Error struct {
	Status        int
	Code          string
	Message       string
	Versions      []SecretVersionSummary
	EnvironmentID string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("Cursor API returned HTTP %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if hint := remediation(e.Code); hint != "" {
		msg += " " + hint
	}
	if e.EnvironmentID != "" {
		msg += " Existing environment id: " + e.EnvironmentID + "."
	}
	if len(e.Versions) > 0 {
		ids := make([]string, 0, len(e.Versions))
		for _, version := range e.Versions {
			if version.ID != "" {
				ids = append(ids, version.ID)
			}
		}
		if len(ids) > 0 {
			msg += " Version ids: " + strings.Join(ids, ", ") + "."
		}
	}
	return msg
}

// IsNotFound reports whether err is, or wraps, an API error with HTTP status 404.
func IsNotFound(err error) bool {
	return IsStatus(err, 404)
}

// IsStatus reports whether err is, or wraps, an API error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// HasCode reports whether err is, or wraps, an API error with the given machine-readable code.
func HasCode(err error, code string) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// decodeError builds an Error from a response body. With redact set, only the
// structured code and message fields are used and the raw body is never copied.
func decodeError(status int, body []byte, redact bool) error {
	apiErr := &Error{Status: status}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) == nil {
		var detail struct {
			Code          json.RawMessage        `json:"code"`
			Message       json.RawMessage        `json:"message"`
			Versions      []SecretVersionSummary `json:"versions"`
			EnvironmentID string                 `json:"environmentId"`
		}
		if raw := top["error"]; len(raw) > 0 {
			if json.Unmarshal(raw, &detail) == nil {
				apiErr.Code = rawString(detail.Code)
				apiErr.Message = rawString(detail.Message)
				apiErr.Versions = detail.Versions
				apiErr.EnvironmentID = strings.TrimSpace(detail.EnvironmentID)
			} else {
				apiErr.Message = rawString(raw)
			}
		}
		if apiErr.Code == "" {
			apiErr.Code = rawString(top["code"])
		}
		if apiErr.Message == "" {
			apiErr.Message = rawString(top["message"])
		}
	}
	apiErr.Code = clip(apiErr.Code, maxRawErrorBytes)
	apiErr.Message = clip(apiErr.Message, maxErrorMessageBytes)
	if apiErr.Message == "" && !redact {
		apiErr.Message = clip(strings.TrimSpace(string(body)), maxRawErrorBytes)
	}
	if apiErr.Message == "" && redact {
		apiErr.Message = "request failed"
	}
	return apiErr
}

// rawString returns the trimmed value of a JSON string, or "" for any other JSON type.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// clip shortens text to at most limit bytes of valid UTF-8. Diagnostics travel over
// gRPC, which rejects strings that are not valid UTF-8, so a cut never ends inside
// a multi-byte character.
func clip(text string, limit int) string {
	if len(text) > limit {
		text = text[:limit]
	}
	return strings.ToValidUTF8(text, "")
}

func remediation(code string) string {
	switch code {
	case "team_admin_required":
		return "The team setting that allows only admins to change secrets rejected this key."
	case "secret_name_ambiguous":
		return "The name has several versions. Import or address one version id instead of guessing."
	case "secret_name_conflict":
		return "A secret with this name already exists and differs only by case."
	case "secret_scope_conflict":
		return "This version's repositories overlap another version of the same name."
	case "feature_unavailable":
		return "The feature is not enabled for this account."
	case "repository_access":
		return "The API key cannot access one of the repositories."
	case "environment_name_conflict":
		return "The owner already has an environment with this name. Choose another name; replacing an environment changes its id."
	case "validation_error":
		return "The request failed validation."
	default:
		return ""
	}
}
