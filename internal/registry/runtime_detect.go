package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
)

// Runtime constants align with OpenTelemetry Operator inject annotations:
//   instrumentation.opentelemetry.io/inject-<runtime>: "true"
const (
	RuntimeJava   = "java"
	RuntimePython = "python"
	RuntimeNodeJS = "nodejs"
	RuntimeDotNet = "dotnet"
	RuntimeGo     = "go"
)

// InspectRuntime queries the registry v2 API and returns the detected runtime
// based on image config (env vars, entrypoint, base image hints, OCI labels).
// Returns "" if no signal is found.
func InspectRuntime(registryHost, name, tag string) string {
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registryHost, name, tag)

	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil || m.Config.Digest == "" {
		return ""
	}

	blobURL := fmt.Sprintf("http://%s/v2/%s/blobs/%s", registryHost, name, m.Config.Digest)
	blobResp, err := httpClient.Get(blobURL)
	if err != nil || blobResp.StatusCode != 200 {
		return ""
	}
	defer blobResp.Body.Close()

	blobBody, err := ioutil.ReadAll(blobResp.Body)
	if err != nil {
		return ""
	}

	var cfg runtimeImageConfig
	if err := json.Unmarshal(blobBody, &cfg); err != nil {
		return ""
	}
	return detectRuntimeFromImageConfig(&cfg.Config)
}

type runtimeImageConfig struct {
	Config runtimeContainerConfig `json:"config"`
}

type runtimeContainerConfig struct {
	Env        []string          `json:"Env"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Labels     map[string]string `json:"Labels"`
}

// detectRuntimeFromImageConfig inspects the parsed image config in priority order:
// explicit OCI label > env var hints > entrypoint/cmd binary names.
func detectRuntimeFromImageConfig(cfg *runtimeContainerConfig) string {
	// 1. Explicit OCI / platform label wins.
	if v := cfg.Labels["com.easydeploy.runtime"]; v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	if v := cfg.Labels["org.opencontainers.image.runtime"]; v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}

	// 2. Env vars are reliable per-language indicators.
	for _, env := range cfg.Env {
		upper := strings.ToUpper(env)
		switch {
		case strings.HasPrefix(upper, "JAVA_HOME="),
			strings.HasPrefix(upper, "JAVA_VERSION="),
			strings.HasPrefix(upper, "JDK_VERSION="):
			return RuntimeJava
		case strings.HasPrefix(upper, "PYTHON_VERSION="),
			strings.HasPrefix(upper, "PYTHONPATH="):
			return RuntimePython
		case strings.HasPrefix(upper, "NODE_VERSION="),
			strings.HasPrefix(upper, "NPM_CONFIG_"):
			return RuntimeNodeJS
		case strings.HasPrefix(upper, "DOTNET_VERSION="),
			strings.HasPrefix(upper, "ASPNETCORE_"),
			strings.HasPrefix(upper, "DOTNET_RUNNING_IN_CONTAINER="):
			return RuntimeDotNet
		}
	}

	// 3. Entrypoint / Cmd binary name.
	if r := runtimeFromArgs(cfg.Entrypoint); r != "" {
		return r
	}
	if r := runtimeFromArgs(cfg.Cmd); r != "" {
		return r
	}
	return ""
}

func runtimeFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	bin := args[0]
	// Strip path: /usr/bin/java → java
	if idx := strings.LastIndex(bin, "/"); idx >= 0 {
		bin = bin[idx+1:]
	}
	bin = strings.ToLower(bin)
	switch {
	case bin == "java", strings.HasSuffix(bin, "javaw"):
		return RuntimeJava
	case strings.HasPrefix(bin, "python"):
		return RuntimePython
	case bin == "node", bin == "nodejs":
		return RuntimeNodeJS
	case bin == "dotnet":
		return RuntimeDotNet
	}
	return ""
}

// RuntimeFromDockerfile fetches Dockerfile from GitHub raw and detects runtime
// from FROM lines (multi-stage aware: returns the most specific signal across all stages).
// Returns "" if no signal is found.
func RuntimeFromDockerfile(owner, repo, ref, dockerfilePath string) string {
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
		if rt := parseDockerfileRuntime(string(body)); rt != "" {
			return rt
		}
	}
	return ""
}

// parseDockerfileRuntime walks every FROM line and picks the strongest runtime hint.
// Multi-stage builds often end on distroless/scratch — we still want the language
// signal from the builder stage (e.g. golang:1.22 → "go").
func parseDockerfileRuntime(content string) string {
	lines := strings.Split(content, "\n")
	var detected string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			continue
		}
		rest := strings.TrimSpace(line[5:])
		// Strip "AS <stage>" suffix.
		if idx := strings.Index(strings.ToUpper(rest), " AS "); idx >= 0 {
			rest = rest[:idx]
		}
		base := strings.ToLower(strings.TrimSpace(rest))
		if rt := runtimeFromBaseImage(base); rt != "" && detected == "" {
			detected = rt
		}
	}
	return detected
}

func runtimeFromBaseImage(base string) string {
	switch {
	case strings.Contains(base, "openjdk"),
		strings.Contains(base, "eclipse-temurin"),
		strings.Contains(base, "amazoncorretto"),
		strings.Contains(base, "azul/zulu"),
		strings.Contains(base, "ibm-semeru"),
		strings.HasPrefix(base, "tomcat:"),
		strings.HasPrefix(base, "jetty:"):
		return RuntimeJava
	case strings.HasPrefix(base, "python:"),
		strings.Contains(base, "/python:"),
		strings.HasPrefix(base, "pypy:"):
		return RuntimePython
	case strings.HasPrefix(base, "node:"),
		strings.Contains(base, "/node:"),
		strings.HasPrefix(base, "nodejs:"):
		return RuntimeNodeJS
	case strings.Contains(base, "mcr.microsoft.com/dotnet"):
		return RuntimeDotNet
	case strings.HasPrefix(base, "golang:"),
		strings.Contains(base, "/golang:"):
		return RuntimeGo
	}
	return ""
}
