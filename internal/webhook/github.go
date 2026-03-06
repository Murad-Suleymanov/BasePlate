package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

func StartServer(ctx context.Context, addr string, handler *GitHubHandler) error {
	mux := http.NewServeMux()
	mux.Handle("/webhook/github", handler)
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
