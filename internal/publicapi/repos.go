package publicapi

import (
	"slices"
	"strings"
)

// CanonicalRepo reduces a repository identifier to lowercase host/owner/repo.
// A value with no host, such as acme/api, is treated as a GitHub repository.
func CanonicalRepo(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "git@") {
		rest := s[strings.Index(s, "@")+1:]
		if host, path, ok := strings.Cut(rest, ":"); ok {
			s = host + "/" + path
		}
	}
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	s = strings.ToLower(s)
	parts := splitPath(s)
	if len(parts) > 0 && len(parts) <= 2 && !strings.Contains(parts[0], ".") && parts[0] != "localhost" {
		s = "github.com/" + s
	}
	return s
}

// CanonicalRepos returns the identifiers the secrets API stores: lowercase, sorted, host/owner/repo.
func CanonicalRepos(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, 0, len(in))
	for _, repo := range in {
		if c := CanonicalRepo(repo); c != "" {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// RepoOwner returns the owner segment of a repository, organization, or URL.
func RepoOwner(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "git@") {
		rest := s[strings.Index(s, "@")+1:]
		if _, path, ok := strings.Cut(rest, ":"); ok {
			s = path
		}
	}
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = s[len(prefix):]
			break
		}
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	parts := splitPath(s)
	if len(parts) == 0 {
		return ""
	}
	if strings.Contains(parts[0], ".") || strings.EqualFold(parts[0], "localhost") {
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	}
	return parts[0]
}

func splitPath(s string) []string {
	raw := strings.Split(s, "/")
	out := make([]string, 0, len(raw))
	for _, part := range raw {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
