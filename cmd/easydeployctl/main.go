package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

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

	Port     int32  `json:"port" yaml:"port"`
	Replicas int32  `json:"replicas" yaml:"replicas"`
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

	if s.Name == "" {
		fmt.Fprintln(os.Stderr, "simple yaml: name is required")
		os.Exit(2)
	}
	if s.Namespace == "" {
		s.Namespace = "default"
	}

	var replicas *int32
	if s.Replicas > 0 {
		r := s.Replicas
		replicas = &r
	}

	var port *int32
	if s.Port > 0 {
		p := s.Port
		port = &p
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
			Image:    s.Image,
			Repo:     s.Repo,
			Tag:      s.Tag,
			Replicas: replicas,
			Port:     port,
			Hostname: s.Hostname,
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
