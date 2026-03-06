package registry

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

type manifest struct {
	Config manifestConfig `json:"config"`
}

type manifestConfig struct {
	Digest string `json:"digest"`
}

type imageConfig struct {
	Config containerConfig `json:"config"`
}

type containerConfig struct {
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
}

// InspectPort queries the registry v2 API and returns the first EXPOSE port
// from the image config. Returns 0 if no port is found or on error.
func InspectPort(registryHost, name, tag string) int32 {
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registryHost, name, tag)

	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return 0
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil || m.Config.Digest == "" {
		return 0
	}

	blobURL := fmt.Sprintf("http://%s/v2/%s/blobs/%s", registryHost, name, m.Config.Digest)
	blobResp, err := httpClient.Get(blobURL)
	if err != nil || blobResp.StatusCode != 200 {
		return 0
	}
	defer blobResp.Body.Close()

	blobBody, err := ioutil.ReadAll(blobResp.Body)
	if err != nil {
		return 0
	}

	var cfg imageConfig
	if err := json.Unmarshal(blobBody, &cfg); err != nil {
		return 0
	}

	for portSpec := range cfg.Config.ExposedPorts {
		port := strings.Split(portSpec, "/")[0]
		if p, err := strconv.Atoi(port); err == nil && p > 0 {
			return int32(p)
		}
	}

	for _, env := range cfg.Config.Env {
		if strings.HasPrefix(env, "PORT=") {
			val := strings.TrimPrefix(env, "PORT=")
			if p, err := strconv.Atoi(val); err == nil && p > 0 {
				return int32(p)
			}
		}
	}

	if p := parsePortFromArgs(cfg.Config.Cmd); p > 0 {
		return p
	}
	if p := parsePortFromArgs(cfg.Config.Entrypoint); p > 0 {
		return p
	}

	return 0
}

func parsePortFromArgs(args []string) int32 {
	portFlags := []string{"--port", "-p", "--bind", "--listen"}
	for i, arg := range args {
		for _, flag := range portFlags {
			if arg == flag && i+1 < len(args) {
				if p, err := strconv.Atoi(args[i+1]); err == nil && p > 0 {
					return int32(p)
				}
			}
			if strings.HasPrefix(arg, flag+"=") {
				val := strings.SplitN(arg, "=", 2)[1]
				if p, err := strconv.Atoi(val); err == nil && p > 0 {
					return int32(p)
				}
			}
		}
	}
	return 0
}
