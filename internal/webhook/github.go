package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	"easy-deploy/internal/injector"
)

const annotRebuild = "deploy.easydeploy.io/rebuild"

// labelApp groups all instances of one app — the controller stamps it on every
// deployment so a single build-complete notification rolls every instance.
const labelApp = "deploy.easydeploy.io/app"

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

const defaultRegistryURL = "registry.registry.svc.cluster.local:5000"

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

	registryURL := strings.TrimSpace(os.Getenv("REGISTRY_URL"))
	if registryURL == "" {
		registryURL = defaultRegistryURL
	}
	registryURL = strings.TrimSuffix(registryURL, "/")
	image := registryURL + "/" + payload.Service + ":" + payload.Tag
	l.Info("build complete, fanning out", "app", payload.Service, "namespace", payload.Namespace, "image", image)

	// payload.Service is the app/repo name shared by every instance. Update the
	// BuildTag on each BirService of this app so the controller keeps the new image
	// on the next reconcile (otherwise it would revert the direct patch below).
	var bsList deployv1alpha1.BirServiceList
	if err := h.Client.List(ctx, &bsList, client.InNamespace(payload.Namespace)); err == nil {
		for i := range bsList.Items {
			b := &bsList.Items[i]
			if _, repo, ok := injector.ParseGitHubRepo(b.Spec.Repo); !ok || repo != payload.Service {
				continue
			}
			b.Status.BuildTag = payload.Tag
			b.Status.BuildStatus = "Succeeded"
			b.Status.BuildImage = image
			if err := h.Client.Status().Update(ctx, b); err != nil {
				l.Error(err, "failed to update BirService build status", "birservice", b.Name)
			}
		}
	} else {
		l.Error(err, "failed to list BirServices", "namespace", payload.Namespace)
	}

	// Patch every instance's deployment directly so all of them roll to the new
	// image at once (an image change triggers a rolling restart).
	jsonPatch := []byte(`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"` + image + `"}]`)
	patched := 0
	var deps appsv1.DeploymentList
	if err := h.Client.List(ctx, &deps, client.InNamespace(payload.Namespace), client.MatchingLabels{labelApp: payload.Service}); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			if err := h.Client.Patch(ctx, d, client.RawPatch(types.JSONPatchType, jsonPatch)); err != nil && !apierrors.IsNotFound(err) {
				l.Error(err, "failed to patch deployment", "deployment", d.Name)
			} else if err == nil {
				patched++
			}
		}
	} else {
		l.Error(err, "failed to list deployments", "namespace", payload.Namespace)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"app":"%s","instances":%d,"tag":"%s"}`+"\n", payload.Service, patched, payload.Tag)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.FromContext(ctx).Info("starting webhook server", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
