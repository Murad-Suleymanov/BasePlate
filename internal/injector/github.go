package injector

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/jefflinse/githubsecret"
)

var githubURLRe = regexp.MustCompile(`(?i)^https?://(?:www\.)?github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseGitHubRepo extracts "owner/repo" from a GitHub URL.
func ParseGitHubRepo(url string) (owner, repo string, ok bool) {
	url = strings.TrimSuffix(strings.ToLower(url), "/")
	m := githubURLRe.FindStringSubmatch(url)
	if len(m) != 3 {
		return "", "", false
	}
	repo = strings.TrimSuffix(m[2], ".git")
	return m[1], repo, true
}

// EnsureRepoSecrets creates REGISTRY_USERNAME and REGISTRY_PASSWORD in the repo's GitHub Actions secrets.
func EnsureRepoSecrets(token, owner, repo, username, password string) error {
	if token == "" || username == "" || password == "" {
		return nil
	}
	key, err := getRepoPublicKey(token, owner, repo)
	if err != nil {
		return fmt.Errorf("get public key: %w", err)
	}
	for name, value := range map[string]string{"REGISTRY_USERNAME": username, "REGISTRY_PASSWORD": password} {
		enc, err := githubsecret.Encrypt(key.Key, value)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", name, err)
		}
		if err := putRepoSecret(token, owner, repo, name, key.KeyID, enc); err != nil {
			return fmt.Errorf("put %s: %w", name, err)
		}
	}
	return nil
}

type repoPublicKey struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

func getRepoPublicKey(token, owner, repo string) (*repoPublicKey, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/secrets/public-key", owner, repo)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var key repoPublicKey
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, string(rb))
	}
	if err := json.NewDecoder(resp.Body).Decode(&key); err != nil {
		return nil, err
	}
	return &key, nil
}

func putRepoSecret(token, owner, repo, name, keyID, encryptedValue string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/secrets/%s", owner, repo, name)
	body, _ := json.Marshal(map[string]string{"key_id": keyID, "encrypted_value": encryptedValue})
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 204 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// EnsureWorkflow creates or updates .github/workflows/build-push.yaml in the repo.
// Requires GITHUB_TOKEN with contents: write.
func EnsureWorkflow(token, owner, repo, registryURL, imageName, webhookURL, serviceName, namespace string) error {
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN is required for pipeline injection")
	}
	if registryURL == "" {
		registryURL = "registry.easysolution.work"
	}
	if imageName == "" {
		imageName = repo
	}
	if webhookURL == "" {
		webhookURL = "https://webhook.easysolution.work"
	}

	content := workflowContent(registryURL, imageName, webhookURL, serviceName, namespace)
	enc := base64.StdEncoding.EncodeToString([]byte(content))

	path := ".github/workflows/build-push.yaml"
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, path)

	// Check if file exists (GET)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body := struct {
		SHA  string `json:"sha"`
		SHA256 string `json:"sha256,omitempty"`
	}{}
	hasExisting := resp.StatusCode == 200
	if hasExisting {
		_ = json.NewDecoder(resp.Body).Decode(&body)
	}

	// PUT to create or update
	putBody := map[string]interface{}{
		"message": "chore: add Easy Deploy build pipeline",
		"content": enc,
	}
	if body.SHA != "" {
		putBody["sha"] = body.SHA
	}

	b, _ := json.Marshal(putBody)
	req2, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Accept", "application/vnd.github.v3+json")
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != 200 && resp2.StatusCode != 201 {
		rb, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("GitHub API %d: %s", resp2.StatusCode, string(rb))
	}
	return nil
}

func workflowContent(registry, imageName, webhookURL, serviceName, namespace string) string {
	notifyStep := ""
	if serviceName != "" && namespace != "" && webhookURL != "" {
		notifyStep = `
      - name: Notify deploy
        run: |
          curl -sf -X POST ` + webhookURL + `/webhook/build-complete \
            -H "Content-Type: application/json" \
            -d '{"service":"` + serviceName + `","namespace":"` + namespace + `","tag":"${{ steps.vars.outputs.short_sha }}"}'
`
	}
	return `# Injected by Easy Deploy — builds and pushes to platform registry
name: Build & Push to Easy Deploy Registry

on:
  push:
    branches: [main, master]

env:
  REGISTRY: ` + registry + `
  IMAGE_NAME: "` + imageName + `"

jobs:
  build-and-push:
    runs-on: ubuntu-latest
    permissions:
      contents: read

    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set tags
        id: vars
        run: |
          echo "short_sha=${GITHUB_SHA::7}" >> $GITHUB_OUTPUT

      - name: Login to Easy Deploy Registry
        uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ secrets.REGISTRY_USERNAME }}
          password: ${{ secrets.REGISTRY_PASSWORD }}

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: |
            ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:${{ steps.vars.outputs.short_sha }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
` + notifyStep + `
`
}
