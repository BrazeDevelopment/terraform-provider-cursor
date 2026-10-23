package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type adminStatusError struct {
	API     string
	Status  int
	Code    string
	Message string
}

func (e *adminStatusError) Error() string {
	msg := fmt.Sprintf("%s returned HTTP %d", e.API, e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (c *apiClient) requireTeamKey() error {
	if c == nil || strings.TrimSpace(c.teamAPIKey) == "" {
		return fmt.Errorf("team_api_key is required; set the provider team_api_key attribute or %s", envTeamAPIKey)
	}
	return nil
}

func (c *apiClient) requireOrgKey() error {
	if err := c.requireOrganization(); err != nil {
		return err
	}
	if strings.TrimSpace(c.orgAPIKey) == "" {
		return fmt.Errorf("organization_api_key is required; set the provider organization_api_key attribute or %s", envOrgAPIKey)
	}
	return nil
}

func (c *apiClient) requireOrganization() error {
	if c == nil || strings.TrimSpace(c.organizationID) == "" {
		return fmt.Errorf("organization_id is required; set the provider organization_id attribute or %s", envOrgID)
	}
	return nil
}

func (c *apiClient) adminDo(ctx context.Context, apiName, key, method, path string, payload any, dest any, success ...int) error {
	if c == nil || c.httpClient == nil {
		return fmt.Errorf("%s client is not configured", apiName)
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("%s key is not configured", apiName)
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encoding %s request: %w", apiName, err)
		}
		body = bytes.NewReader(raw)
	}
	base := strings.TrimRight(strings.TrimSpace(c.adminBase), "/")
	if base == "" {
		base = defaultAdminAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return fmt.Errorf("building %s request: %w", apiName, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", basicAuthHeader(key))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", apiName, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading %s response: %w", apiName, err)
	}
	if !adminStatusOK(resp.StatusCode, success) {
		code, message := adminErrorParts(raw)
		return &adminStatusError{API: apiName, Status: resp.StatusCode, Code: code, Message: message}
	}
	if dest != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, dest); err != nil {
			return fmt.Errorf("decoding %s response: %w", apiName, err)
		}
	}
	return nil
}

func adminStatusOK(status int, success []int) bool {
	if len(success) == 0 {
		return status >= 200 && status < 300
	}
	for _, code := range success {
		if status == code {
			return true
		}
	}
	return false
}

func adminErrorParts(body []byte) (string, string) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		text := strings.TrimSpace(string(body))
		if len(text) > 512 {
			text = text[:512]
		}
		return "", text
	}
	code := jsonString(top["code"])
	message := jsonString(top["message"])
	if raw, ok := top["error"]; ok {
		if s := jsonString(raw); s != "" {
			if message == "" {
				message = s
			}
		} else {
			var obj struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				if code == "" {
					code = obj.Code
				}
				if message == "" {
					message = obj.Message
				}
			}
		}
	}
	return strings.TrimSpace(code), strings.TrimSpace(message)
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func annotateModelAccessConflict(err error) error {
	var apiErr *adminStatusError
	if err != nil && asAdmin(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return fmt.Errorf("%w. Apply cursor_model_access_configuration first; provider and model reads and writes return 409 while configuration state is unrestricted or legacy", err)
	}
	return err
}

func asAdmin(err error, dest **adminStatusError) bool {
	apiErr, ok := err.(*adminStatusError)
	if ok {
		*dest = apiErr
	}
	return ok
}

type teamMemberRecord struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	IsRemoved bool   `json:"isRemoved"`
}

func (c *apiClient) listTeamMemberRecords(ctx context.Context) ([]teamMemberRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	var members []teamMemberRecord
	err := c.eachAdminPage(ctx, "Team Admin API", c.teamAPIKey, adminTeamMembersPath, func(body []byte) (*adminPagination, error) {
		var page struct {
			TeamMembers []teamMemberRecord `json:"teamMembers"`
			Pagination  *adminPagination   `json:"pagination"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding team members: %w", err)
		}
		members = append(members, page.TeamMembers...)
		return page.Pagination, nil
	})
	return members, err
}

type modelAccessConfiguration struct {
	TeamID             int64   `json:"teamId"`
	State              string  `json:"state"`
	NewProviderDefault *string `json:"newProviderDefault"`
	NewModelDefault    *string `json:"newModelDefault"`
}

func (c *apiClient) getModelAccessConfiguration(ctx context.Context) (*modelAccessConfiguration, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	var out modelAccessConfiguration
	if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodGet, "/teams/model-access/configuration", nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *apiClient) putModelAccessConfiguration(ctx context.Context, payload any) (*modelAccessConfiguration, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	var out modelAccessConfiguration
	if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPut, "/teams/model-access/configuration", payload, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

type modelAccessProviderRecord struct {
	ID          string                   `json:"id"`
	DisplayName string                   `json:"displayName"`
	Enabled     bool                     `json:"enabled"`
	Models      []modelAccessModelRecord `json:"models"`
}

type modelAccessModelRecord struct {
	ID          string                       `json:"id"`
	DisplayName string                       `json:"displayName"`
	Enabled     bool                         `json:"enabled"`
	Provider    string                       `json:"provider"`
	Parameters  []modelAccessParameterRecord `json:"parameters"`
}

type modelAccessParameterRecord struct {
	ID                     string   `json:"id"`
	DisplayName            string   `json:"displayName"`
	SupportedValues        []string `json:"supportedValues"`
	AllowedValues          []string `json:"allowedValues"`
	ConfiguredDefaultValue *string  `json:"configuredDefaultValue"`
	CatalogDefaultValue    *string  `json:"catalogDefaultValue"`
}

func (c *apiClient) listModelAccessProviders(ctx context.Context) ([]modelAccessProviderRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	var out struct {
		Providers []modelAccessProviderRecord `json:"providers"`
	}
	err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodGet, "/teams/model-access/providers", nil, &out, http.StatusOK)
	if err != nil {
		return nil, annotateModelAccessConflict(err)
	}
	return out.Providers, nil
}

func (c *apiClient) putModelAccessProvider(ctx context.Context, providerID string, enabled bool) error {
	if err := c.requireTeamKey(); err != nil {
		return err
	}
	path := "/teams/model-access/providers/" + url.PathEscape(providerID)
	err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPut, path, map[string]any{"enabled": enabled}, nil, http.StatusOK, http.StatusNoContent)
	return annotateModelAccessConflict(err)
}

func (c *apiClient) listModelAccessModels(ctx context.Context, providerID string) ([]modelAccessModelRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	path := "/teams/model-access/providers/" + url.PathEscape(providerID) + "/models"
	var out struct {
		Models []modelAccessModelRecord `json:"models"`
	}
	err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodGet, path, nil, &out, http.StatusOK)
	if err != nil {
		return nil, annotateModelAccessConflict(err)
	}
	if len(out.Models) > 0 {
		return out.Models, nil
	}
	providers, err := c.listModelAccessProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		if provider.ID == providerID {
			return provider.Models, nil
		}
	}
	return nil, &adminStatusError{API: "Team Admin API", Status: http.StatusNotFound, Message: fmt.Sprintf("provider %q was not found", providerID)}
}

func (c *apiClient) putModelAccessModel(ctx context.Context, providerID, modelID string, payload any) (*modelAccessModelRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	path := "/teams/model-access/providers/" + url.PathEscape(providerID) + "/models/" + url.PathEscape(modelID)
	var out modelAccessModelRecord
	err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPut, path, payload, &out, http.StatusOK)
	if err != nil {
		return nil, annotateModelAccessConflict(err)
	}
	return &out, nil
}

type spendMember struct {
	Email               string `json:"email"`
	MonthlyLimitDollars *int64 `json:"monthlyLimitDollars"`
}

func (c *apiClient) findUserSpend(ctx context.Context, email string) (*int64, bool, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, false, err
	}
	want := strings.TrimSpace(email)
	for page := 1; page <= maxAdminPages; page++ {
		payload := map[string]any{"page": page, "pageSize": adminPageSize, "searchTerm": want}
		var out struct {
			TeamMemberSpend []spendMember `json:"teamMemberSpend"`
			TotalPages      int           `json:"totalPages"`
		}
		if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPost, "/teams/spend", payload, &out, http.StatusOK); err != nil {
			return nil, false, err
		}
		for _, member := range out.TeamMemberSpend {
			if strings.EqualFold(strings.TrimSpace(member.Email), want) {
				return member.MonthlyLimitDollars, true, nil
			}
		}
		if out.TotalPages == 0 || page >= out.TotalPages {
			return nil, false, nil
		}
	}
	return nil, false, fmt.Errorf("Team Admin API spend list exceeded %d pages", maxAdminPages)
}

func (c *apiClient) setUserSpendLimit(ctx context.Context, email string, limit *int64) error {
	if err := c.requireTeamKey(); err != nil {
		return err
	}
	payload := map[string]any{"userEmail": email, "spendLimitDollars": nil}
	if limit != nil {
		payload["spendLimitDollars"] = *limit
	}
	var out struct {
		Outcome string `json:"outcome"`
		Message string `json:"message"`
	}
	if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPost, "/teams/user-spend-limit", payload, &out, http.StatusOK); err != nil {
		return err
	}
	if strings.EqualFold(out.Outcome, "error") {
		return fmt.Errorf("set user spend limit: %s", out.Message)
	}
	return nil
}

type cursorGroup struct {
	ID                          string `json:"id"`
	PublicID                    string `json:"publicId"`
	Name                        string `json:"name"`
	MemberCount                 int64  `json:"memberCount"`
	MonthlySpendingLimitDollars *int64 `json:"monthlySpendingLimitDollars"`
	CreatedAt                   string `json:"createdAt"`
	UpdatedAt                   string `json:"updatedAt"`
}

type groupMemberRecord struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

func (c *apiClient) createCursorGroup(ctx context.Context, apiName, key, collection, name string) (*cursorGroup, error) {
	var out struct {
		Group cursorGroup `json:"group"`
	}
	if err := c.adminDo(ctx, apiName, key, http.MethodPost, collection, map[string]string{"name": name}, &out, http.StatusCreated, http.StatusOK); err != nil {
		return nil, err
	}
	return &out.Group, nil
}

func (c *apiClient) getCursorGroup(ctx context.Context, apiName, key, collection, id string) (*cursorGroup, error) {
	var out struct {
		Group cursorGroup `json:"group"`
	}
	if err := c.adminDo(ctx, apiName, key, http.MethodGet, collection+"/"+url.PathEscape(id), nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out.Group, nil
}

func (c *apiClient) patchCursorGroup(ctx context.Context, apiName, key, collection, id string, payload map[string]any) (*cursorGroup, error) {
	var out struct {
		Group cursorGroup `json:"group"`
	}
	if err := c.adminDo(ctx, apiName, key, http.MethodPatch, collection+"/"+url.PathEscape(id), payload, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out.Group, nil
}

func (c *apiClient) deleteCursorGroup(ctx context.Context, apiName, key, collection, id string) error {
	return c.adminDo(ctx, apiName, key, http.MethodDelete, collection+"/"+url.PathEscape(id), nil, nil, http.StatusOK, http.StatusNoContent)
}

func (c *apiClient) listGroupMembers(ctx context.Context, apiName, key, collection, id string) ([]groupMemberRecord, error) {
	var members []groupMemberRecord
	path := collection + "/" + url.PathEscape(id) + "/members"
	for page := 1; page <= maxAdminPages; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(adminPageSize)}}
		var out struct {
			Members    []groupMemberRecord `json:"members"`
			Pagination *adminPagination    `json:"pagination"`
		}
		if err := c.adminDo(ctx, apiName, key, http.MethodGet, path+"?"+query.Encode(), nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		members = append(members, out.Members...)
		if out.Pagination == nil || out.Pagination.HasNextPage == nil || !*out.Pagination.HasNextPage {
			return members, nil
		}
	}
	return nil, fmt.Errorf("%s members list exceeded %d pages", apiName, maxAdminPages)
}

func (c *apiClient) bulkGroupMembers(ctx context.Context, apiName, key, collection, id, action string, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	path := collection + "/" + url.PathEscape(id) + "/members/" + action
	for start := 0; start < len(userIDs); start += 100 {
		end := start + 100
		if end > len(userIDs) {
			end = len(userIDs)
		}
		payload := map[string]any{"userIds": userIDs[start:end]}
		if err := c.adminDo(ctx, apiName, key, http.MethodPost, path, payload, nil, http.StatusOK); err != nil {
			return err
		}
	}
	return nil
}

type repoBlocklistRecord struct {
	ID       string   `json:"id"`
	URL      string   `json:"url"`
	Patterns []string `json:"patterns"`
}

func (c *apiClient) listRepoBlocklists(ctx context.Context) ([]repoBlocklistRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	var out struct {
		Repos []repoBlocklistRecord `json:"repos"`
	}
	if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodGet, "/settings/repo-blocklists/repos", nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Repos, nil
}

func (c *apiClient) upsertRepoBlocklist(ctx context.Context, repoURL string, patterns []string) (*repoBlocklistRecord, error) {
	if err := c.requireTeamKey(); err != nil {
		return nil, err
	}
	payload := map[string]any{"repos": []map[string]any{{"url": repoURL, "patterns": patterns}}}
	var out struct {
		Repos []repoBlocklistRecord `json:"repos"`
	}
	if err := c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodPost, "/settings/repo-blocklists/repos/upsert", payload, &out, http.StatusOK); err != nil {
		return nil, err
	}
	for _, repo := range out.Repos {
		if repo.URL == repoURL {
			return &repo, nil
		}
	}
	if len(out.Repos) == 1 {
		return &out.Repos[0], nil
	}
	return nil, fmt.Errorf("upsert response did not include repository %q", repoURL)
}

func (c *apiClient) deleteRepoBlocklist(ctx context.Context, id string) error {
	if err := c.requireTeamKey(); err != nil {
		return err
	}
	return c.adminDo(ctx, "Team Admin API", c.teamAPIKey, http.MethodDelete, "/settings/repo-blocklists/repos/"+url.PathEscape(id), nil, nil, http.StatusOK, http.StatusNoContent)
}
