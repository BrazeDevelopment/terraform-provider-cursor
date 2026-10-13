package publicapi

import (
	"encoding/json"
	"fmt"
	"strings"
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

func IsNotFound(err error) bool {
	apiErr, ok := err.(*Error)
	return ok && apiErr.Status == 404
}

func IsStatus(err error, status int) bool {
	apiErr, ok := err.(*Error)
	return ok && apiErr.Status == status
}

func HasCode(err error, code string) bool {
	apiErr, ok := err.(*Error)
	return ok && apiErr.Code == code
}

func decodeError(status int, body []byte, redact bool) error {
	apiErr := &Error{Status: status}
	var wrapped struct {
		Error struct {
			Code          string                 `json:"code"`
			Message       string                 `json:"message"`
			Versions      []SecretVersionSummary `json:"versions"`
			EnvironmentID string                 `json:"environmentId"`
		} `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		apiErr.Code = strings.TrimSpace(wrapped.Error.Code)
		apiErr.Message = strings.TrimSpace(wrapped.Error.Message)
		apiErr.Versions = wrapped.Error.Versions
		apiErr.EnvironmentID = wrapped.Error.EnvironmentID
		if apiErr.Code == "" {
			apiErr.Code = strings.TrimSpace(wrapped.Code)
		}
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(wrapped.Message)
		}
	}
	if apiErr.Message == "" && !redact {
		text := strings.TrimSpace(string(body))
		if len(text) > 512 {
			text = text[:512]
		}
		apiErr.Message = text
	}
	if apiErr.Message == "" && redact {
		apiErr.Message = "request failed"
	}
	return apiErr
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
