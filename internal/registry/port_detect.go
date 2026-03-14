package registry

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var dfHttpClient = &http.Client{Timeout: 10 * time.Second}

// PortFromDockerfile fetches Dockerfile from GitHub raw and parses EXPOSE / CMD --port.
// owner, repo, ref (branch/tag), dockerfilePath (e.g. "Dockerfile").
// Returns 0 if not found or on error.
func PortFromDockerfile(owner, repo, ref, dockerfilePath string) int32 {
	if ref == "" {
		ref = "main"
	}
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	for _, r := range []string{ref, "main", "master"} {
		url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, r, dockerfilePath)
		resp, err := dfHttpClient.Get(url)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if p := parseDockerfilePort(string(body)); p > 0 {
			return p
		}
	}
	return 0
}

func parseDockerfilePort(content string) int32 {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "EXPOSE ") {
			rest := strings.TrimSpace(line[6:])
			portStr := strings.Split(rest, "/")[0]
			portStr = strings.TrimSpace(portStr)
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p < 65536 {
				return int32(p)
			}
		}
	}
	// CMD / ENTRYPOINT with --port N or -p N (JSON: "--port", "8000" or exec: --port 8000)
	rePort := regexp.MustCompile(`(?:--port|-p)[\s=",]*(\d{1,5})`)
	for _, line := range lines {
		if matches := rePort.FindStringSubmatch(line); len(matches) >= 2 {
			if p, err := strconv.Atoi(matches[1]); err == nil && p > 0 {
				return int32(p)
			}
		}
	}
	return 0
}
