package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

type SimpleServiceYAML struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace" yaml:"namespace"`

	// image: ghcr.io/acme/hello:1.0.0 (optional)
	Image string `json:"image" yaml:"image"`

	// repo: ghcr.io/acme/hello, tag: 1.0.0 (optional)
	Repo string `json:"repo" yaml:"repo"`
	Tag  string `json:"tag" yaml:"tag"`

	Port     int32 `json:"port" yaml:"port"`
	Replicas int32 `json:"replicas" yaml:"replicas"`
	// Legacy top-level HPA fields (backward compatibility)
	MinReplicas int32 `json:"minReplicas" yaml:"minReplicas"`
	MaxReplicas int32 `json:"maxReplicas" yaml:"maxReplicas"`
	HPA         struct {
		MinReplicas int32 `json:"minReplicas" yaml:"minReplicas"`
		MaxReplicas int32 `json:"maxReplicas" yaml:"maxReplicas"`
	} `json:"hpa" yaml:"hpa"`
	Resources struct {
		Requests struct {
			Memory string `json:"memory" yaml:"memory"`
			CPU    string `json:"cpu" yaml:"cpu"`
		} `json:"requests" yaml:"requests"`
		Limits struct {
			Memory string `json:"memory" yaml:"memory"`
			CPU    string `json:"cpu" yaml:"cpu"`
		} `json:"limits" yaml:"limits"`
	} `json:"resources" yaml:"resources"`
	Hostname string `json:"hostname" yaml:"hostname"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "generate":
		generate(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  easydeployctl generate -f <simple.yaml> [-o <birservice.yaml>]\n")
}

func generate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	var inPath string
	var outPath string
	fs.StringVar(&inPath, "f", "", "Path to simple YAML")
	fs.StringVar(&outPath, "o", "", "Output path for BirService CR YAML (optional; default stdout)")
	_ = fs.Parse(args)

	if inPath == "" {
		fmt.Fprintln(os.Stderr, "missing -f")
		os.Exit(2)
	}

	b, err := ioutil.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(1)
	}

	var s SimpleServiceYAML
	if err := yaml.Unmarshal(b, &s); err != nil {
		fmt.Fprintf(os.Stderr, "parse yaml: %v\n", err)
		os.Exit(1)
	}

	// Derive name/namespace from path (service_name/namespace_name.yaml) if not in YAML
	dir, file := filepath.Dir(inPath), filepath.Base(inPath)
	namespaceFromFile := strings.TrimSuffix(file, filepath.Ext(file))
	if s.Name == "" {
		// Path like "api/prod.yaml" -> name=api (from dir), or just "prod.yaml" -> name=prod
		if dir != "." && dir != string(filepath.Separator) {
			s.Name = filepath.Base(dir)
		} else {
			s.Name = namespaceFromFile
		}
	}
	if s.Namespace == "" {
		if dir != "." && dir != string(filepath.Separator) {
			s.Namespace = namespaceFromFile
		} else {
			s.Namespace = "default"
		}
	}

	var replicas *int32
	if s.Replicas > 0 {
		r := s.Replicas
		replicas = &r
	}
	var hpa *deployv1alpha1.HPASpec
	min := s.HPA.MinReplicas
	max := s.HPA.MaxReplicas
	if min == 0 {
		min = s.MinReplicas
	}
	if max == 0 {
		max = s.MaxReplicas
	}
	if min > 0 && max > 0 {
		hpa = &deployv1alpha1.HPASpec{
			MinReplicas: &min,
			MaxReplicas: &max,
		}
	}

	var port *int32
	if s.Port > 0 {
		p := s.Port
		port = &p
	}
	var resources *deployv1alpha1.ResourceConfigSpec
	if strings.TrimSpace(s.Resources.Requests.Memory) != "" ||
		strings.TrimSpace(s.Resources.Requests.CPU) != "" ||
		strings.TrimSpace(s.Resources.Limits.Memory) != "" ||
		strings.TrimSpace(s.Resources.Limits.CPU) != "" {
		resources = &deployv1alpha1.ResourceConfigSpec{
			Requests: &deployv1alpha1.ResourceValues{
				Memory: strings.TrimSpace(s.Resources.Requests.Memory),
				CPU:    strings.TrimSpace(s.Resources.Requests.CPU),
			},
			Limits: &deployv1alpha1.ResourceValues{
				Memory: strings.TrimSpace(s.Resources.Limits.Memory),
				CPU:    strings.TrimSpace(s.Resources.Limits.CPU),
			},
		}
	}

	bs := deployv1alpha1.BirService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: deployv1alpha1.Group + "/" + deployv1alpha1.Version,
			Kind:       "BirService",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
		},
		Spec: deployv1alpha1.BirServiceSpec{
			Image:     s.Image,
			Repo:      s.Repo,
			Tag:       s.Tag,
			Replicas:  replicas,
			HPA:       hpa,
			Resources: resources,
			Port:      port,
			Hostname:  s.Hostname,
		},
	}

	out, err := yaml.Marshal(bs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}

	if outPath == "" {
		_, _ = os.Stdout.Write(out)
		return
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := ioutil.WriteFile(outPath, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}
}
