package credentials

import (
	"context"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultRegistryUser = "admin"
	DefaultRegistryPass = "EasyDeploy2026"
)

// PipelineCreds holds credentials for pipeline injection.
type PipelineCreds struct {
	GitHubToken       string
	RegistryUsername  string
	RegistryPassword  string
}

// ResolvePipelineCreds returns credentials for pipeline injection.
// GITHUB_TOKEN: env → ArgoCD repo secret (argocd namespace, github.com).
// REGISTRY_*: env → defaults (admin/EasyDeploy2026).
func ResolvePipelineCreds(ctx context.Context, c client.Client) PipelineCreds {
	out := PipelineCreds{
		RegistryUsername: DefaultRegistryUser,
		RegistryPassword: DefaultRegistryPass,
	}
	if u := getEnv("REGISTRY_USERNAME"); u != "" {
		out.RegistryUsername = u
	}
	if p := getEnv("REGISTRY_PASSWORD"); p != "" {
		out.RegistryPassword = p
	}
	if t := getEnv("GITHUB_TOKEN"); t != "" {
		out.GitHubToken = t
		return out
	}
	// Fallback: ArgoCD repository secret for github.com
	if t := resolveFromArgoCDRepo(ctx, c); t != "" {
		out.GitHubToken = t
	}
	return out
}

func getEnv(k string) string {
	return strings.TrimSpace(os.Getenv(k))
}

func resolveFromArgoCDRepo(ctx context.Context, c client.Client) string {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace("argocd"),
		client.MatchingLabels{"argocd.argoproj.io/secret-type": "repository"}); err != nil {
		return ""
	}
	for i := range list.Items {
		s := &list.Items[i]
		url := getSecretKey(s, "url")
		if url == "" {
			url = getSecretKey(s, "url.1") // sometimes stored differently
		}
		if !strings.Contains(strings.ToLower(url), "github.com") {
			continue
		}
		if p := getSecretKey(s, "password"); p != "" {
			return p
		}
		if b := getSecretKey(s, "bearerToken"); b != "" {
			return b
		}
	}
	return ""
}

func getSecretKey(s *corev1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return strings.TrimSpace(string(v))
	}
	if v, ok := s.StringData[key]; ok {
		return strings.TrimSpace(v)
	}
	return ""
}
