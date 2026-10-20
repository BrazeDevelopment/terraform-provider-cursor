package provider

import (
	"context"
	"slices"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Terraform requires a non-computed attribute to keep its configured value through plan and apply.
// These helpers keep the prior value when the API returns an equivalent spelling and use the API
// value only when it differs, so real drift still shows up in the next plan.

func keepEquivalentRepos(ctx context.Context, prior types.List, api []string) types.List {
	if !prior.IsNull() && !prior.IsUnknown() {
		var configured []string
		if diags := prior.ElementsAs(ctx, &configured, false); !diags.HasError() && slices.Equal(publicapi.CanonicalRepos(configured), publicapi.CanonicalRepos(api)) {
			return prior
		}
	}
	list, _ := types.ListValueFrom(ctx, types.StringType, publicapi.CanonicalRepos(api))
	return list
}

func keepEquivalentEnvironmentRepos(prior []environmentRepoModel, api []publicapi.EnvironmentRepo) []environmentRepoModel {
	configured := make([]string, 0, len(prior))
	for _, repo := range prior {
		if repo.URL.IsNull() || repo.URL.IsUnknown() {
			configured = nil
			break
		}
		configured = append(configured, repo.URL.ValueString())
	}
	returned := make([]string, 0, len(api))
	for _, repo := range api {
		returned = append(returned, repo.URL)
	}
	if prior != nil && configured != nil && slices.Equal(canonicalRepoMultiset(configured), canonicalRepoMultiset(returned)) {
		return prior
	}
	out := make([]environmentRepoModel, 0, len(api))
	for _, repo := range api {
		out = append(out, environmentRepoModel{URL: types.StringValue(repo.URL)})
	}
	return out
}

func canonicalRepoMultiset(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		out = append(out, publicapi.CanonicalRepo(repo))
	}
	slices.Sort(out)
	return out
}

func keepEquivalentJSON(prior types.String, api string) types.String {
	if api == "" {
		return prior
	}
	normalized, err := normalizeEnvironmentJSON(api)
	if err != nil {
		return types.StringValue(api)
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		if configured, err := normalizeEnvironmentJSON(prior.ValueString()); err == nil && configured == normalized {
			return prior
		}
	}
	return types.StringValue(normalized)
}

func keepEquivalentRepoURL(prior types.String, api string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && publicapi.CanonicalRepo(prior.ValueString()) == publicapi.CanonicalRepo(api) {
		return prior
	}
	return types.StringValue(api)
}
