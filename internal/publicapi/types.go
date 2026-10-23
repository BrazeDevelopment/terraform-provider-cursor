package publicapi

// Types match openapi/cloud-agents-openapi.yaml for the environment, secret, model, and API-key operations this provider calls.

type EnvironmentRepo struct {
	URL string `json:"url"`
}

type EnvironmentRepoFile struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

type Environment struct {
	ID              string               `json:"id"`
	Name            string               `json:"name,omitempty"`
	Owner           string               `json:"owner"`
	Repos           []EnvironmentRepo    `json:"repos"`
	CreatedAt       string               `json:"createdAt"`
	UpdatedAt       string               `json:"updatedAt"`
	EnvironmentJSON string               `json:"environmentJson,omitempty"`
	VersionID       string               `json:"versionId,omitempty"`
	RepoFile        *EnvironmentRepoFile `json:"repoFile,omitempty"`
}

type EnvironmentPage struct {
	Items      []Environment `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

type CreateEnvironmentRequest struct {
	Owner           string            `json:"owner"`
	Name            string            `json:"name"`
	Repos           []EnvironmentRepo `json:"repos"`
	EnvironmentJSON string            `json:"environmentJson"`
}

type UpdateEnvironmentRequest struct {
	Name            *string `json:"name,omitempty"`
	EnvironmentJSON *string `json:"environmentJson,omitempty"`
}

type SecretVersion struct {
	ID        string   `json:"id,omitempty"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Repos     []string `json:"repos"`
	CreatedAt string   `json:"createdAt,omitempty"`
}

type SecretVersionSummary struct {
	ID    string   `json:"id"`
	Repos []string `json:"repos"`
}

type SecretPage struct {
	Items      []SecretVersion `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

type SetSecretRequest struct {
	Value string   `json:"value"`
	Type  string   `json:"type,omitempty"`
	Repos []string `json:"repos"`
}

type APIKeyInfo struct {
	APIKeyName    string `json:"apiKeyName"`
	CreatedAt     string `json:"createdAt"`
	UserID        *int64 `json:"userId,omitempty"`
	UserEmail     string `json:"userEmail,omitempty"`
	UserFirstName string `json:"userFirstName,omitempty"`
	UserLastName  string `json:"userLastName,omitempty"`
}

type ModelParameterValue struct {
	Value       string `json:"value"`
	DisplayName string `json:"displayName,omitempty"`
}

type ModelParameter struct {
	ID          string                `json:"id"`
	DisplayName string                `json:"displayName,omitempty"`
	Values      []ModelParameterValue `json:"values"`
}

type ModelVariantParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type ModelVariant struct {
	Params      []ModelVariantParam `json:"params"`
	DisplayName string              `json:"displayName"`
	Description string              `json:"description,omitempty"`
	IsDefault   bool                `json:"isDefault,omitempty"`
}

type Model struct {
	ID          string           `json:"id"`
	DisplayName string           `json:"displayName"`
	Description string           `json:"description,omitempty"`
	Aliases     []string         `json:"aliases,omitempty"`
	Parameters  []ModelParameter `json:"parameters,omitempty"`
	Variants    []ModelVariant   `json:"variants,omitempty"`
}

type ModelPage struct {
	Items []Model `json:"items"`
}

type ActiveBuild struct {
	Type    string `json:"type"`
	BuildID string `json:"buildId,omitempty"`
}

type EnvironmentBuild struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type Pool struct {
	Scope                     string `json:"scope"`
	OwnerID                   int64  `json:"ownerId,omitempty"`
	PoolName                  string `json:"poolName"`
	RepoOwner                 string `json:"repoOwner,omitempty"`
	RepoName                  string `json:"repoName,omitempty"`
	RepoURL                   string `json:"repoUrl,omitempty"`
	ConnectedWorkerCount      int64  `json:"connectedWorkerCount"`
	InUseWorkerCount          int64  `json:"inUseWorkerCount"`
	FirstSeenAtMs             int64  `json:"firstSeenAtMs,omitempty"`
	LastSeenAtMs              int64  `json:"lastSeenAtMs,omitempty"`
	IsStale                   bool   `json:"isStale"`
	WorkerReadyTimeoutSeconds int64  `json:"workerReadyTimeoutSeconds"`
}

type PoolPage struct {
	Pools []Pool `json:"pools"`
}

type RegisterPoolRequest struct {
	Scope                     string `json:"scope"`
	PoolName                  string `json:"poolName"`
	RepoOwner                 string `json:"repoOwner,omitempty"`
	RepoName                  string `json:"repoName,omitempty"`
	RepoURL                   string `json:"repoUrl,omitempty"`
	WorkerReadyTimeoutSeconds int64  `json:"workerReadyTimeoutSeconds"`
}

type WorkerSummaryCounts struct {
	TotalConnected int64 `json:"totalConnected"`
	InUse          int64 `json:"inUse"`
}

type WorkerSummary struct {
	TeamSummary *WorkerSummaryCounts `json:"teamSummary"`
	UserSummary *WorkerSummaryCounts `json:"userSummary"`
}
