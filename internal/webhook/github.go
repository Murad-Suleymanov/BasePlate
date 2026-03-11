package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

const annotRebuild = "deploy.easydeploy.io/rebuild"

type GitHubHandler struct {
	Client client.Client
}

type githubPushPayload struct {
	Ref        string           `json:"ref"`
	Repository githubRepository `json:"repository"`
}

type githubRepository struct {
	CloneURL string `json:"clone_url"`
	HTMLURL  string `json:"html_url"`
}

func (h *GitHubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := log.FromContext(ctx).WithName("webhook")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload githubPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	repoURL := normalizeGitURL(payload.Repository.HTMLURL)
	if repoURL == "" {
		repoURL = normalizeGitURL(payload.Repository.CloneURL)
	}
	if repoURL == "" {
		http.Error(w, "no repository URL in payload", http.StatusBadRequest)
		return
	}

	l.Info("received push event", "repo", repoURL, "ref", payload.Ref)

	var bsList deployv1alpha1.BirServiceList
	if err := h.Client.List(ctx, &bsList); err != nil {
		l.Error(err, "failed to list BirServices")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	updated := 0
	ts := fmt.Sprintf("%d", time.Now().Unix())
	for i := range bsList.Items {
		bs := &bsList.Items[i]
		if normalizeGitURL(bs.Spec.Repo) != repoURL {
			continue
		}

		anns := bs.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[annotRebuild] = ts
		bs.SetAnnotations(anns)

		if err := h.Client.Update(ctx, bs); err != nil {
			l.Error(err, "failed to annotate BirService", "name", bs.Name, "namespace", bs.Namespace)
			continue
		}
		l.Info("triggered rebuild", "birservice", bs.Name, "namespace", bs.Namespace)
		updated++
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"triggered":%d}`, updated)
}

func normalizeGitURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimSuffix(u, "/")
	return strings.ToLower(u)
}

type BuildCompleteHandler struct {
	Client client.Client
}

const inClusterRegistry = "registry.registry.svc.cluster.local:5000"

type buildCompletePayload struct {
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	Tag       string `json:"tag"`
}

func (h *BuildCompleteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := log.FromContext(ctx).WithName("build-complete")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload buildCompletePayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Service == "" || payload.Namespace == "" {
		http.Error(w, "invalid JSON: need {service, namespace}", http.StatusBadRequest)
		return
	}
	if payload.Tag == "" {
		payload.Tag = "latest"
	}

	depName := payload.Service + "-deploy"
	image := inClusterRegistry + "/" + payload.Service + ":" + payload.Tag
	l.Info("build complete, deploying", "deployment", depName, "namespace", payload.Namespace, "image", image)

	// Patch deployment with new image (tag-based, rollback üçün)
	jsonPatch := []byte(`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"` + image + `"}]`)
	dep := &appsv1.Deployment{}
	dep.Name = depName
	dep.Namespace = payload.Namespace
	err = h.Client.Patch(ctx, dep, client.RawPatch(types.JSONPatchType, jsonPatch))
	if err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "deployment not found", http.StatusNotFound)
			return
		}
		l.Error(err, "failed to patch deployment", "deployment", depName)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// BirService status yenilə - controller status.BuildTag istifadə edəcək
	bs := &deployv1alpha1.BirService{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: payload.Service, Namespace: payload.Namespace}, bs); err == nil {
		bs.Status.BuildTag = payload.Tag
		bs.Status.BuildStatus = "Succeeded"
		bs.Status.BuildImage = image
		_ = h.Client.Status().Update(ctx, bs)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"deployment":"%s","tag":"%s"}`+"\n", depName, payload.Tag)
}

func StartServer(ctx context.Context, addr string, handler *GitHubHandler, buildComplete *BuildCompleteHandler) error {
	mux := http.NewServeMux()
	mux.Handle("/webhook/github", handler)
	mux.Handle("/webhook/build-complete", buildComplete)
	mux.HandleFunc("/webhook/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.FromContext(ctx).Info("starting webhook server", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
