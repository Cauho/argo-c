package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReconcilerPagination(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var creates, secondPage int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					atomic.AddInt32(&creates, 1)
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":999,"config":{"url":"https://example.com/desired"}}`)
					return
				}
				if req.URL.Query().Get("page") == "2" {
					atomic.AddInt32(&secondPage, 1)
					w.WriteHeader(status)
					if status == http.StatusOK {
						fmt.Fprint(w, `[{"id":42,"events":["pull_request"],"config":{"url":"https://example.com/desired"}}]`)
					}
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/hooks?page=2>; rel="next", <%s/repos/owner/repo/hooks?page=2>; rel="last"`, server.URL, server.URL))
				// GitHub defaults to 30 entries/page. Keep each event type below its
				// 20-hook limit, while the overall repository spans multiple pages.
				first := make([]Webhook, 30)
				for i := range first {
					first[i] = Webhook{ID: int64(i + 1), Events: []string{[]string{"push", "issues", "release"}[i/10]}, Config: WebhookConfig{URL: fmt.Sprintf("https://example.com/other-%d", i)}}
				}
				json.NewEncoder(w).Encode(first)
			}))
			defer server.Close()
			client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
			hook, err := NewReconciler(client, nil).Reconcile(context.Background(), "owner", "repo", &Webhook{Config: WebhookConfig{URL: "https://example.com/desired"}})
			if got := atomic.LoadInt32(&creates); got != 0 {
				t.Fatalf("created %d webhook(s) before establishing absence across all pages", got)
			}
			if atomic.LoadInt32(&secondPage) != 1 {
				t.Fatal("second page was not read exactly once")
			}
			if status == http.StatusOK {
				if err != nil || hook == nil || hook.ID != 42 {
					t.Fatalf("expected existing webhook 42, got %#v, %v", hook, err)
				}
			} else {
				if err == nil {
					t.Fatal("later-page failure was swallowed")
				}
				if status == http.StatusTooManyRequests && !errors.Is(err, ErrRateLimited) {
					t.Fatalf("lost rate-limit error: %v", err)
				}
				if status == http.StatusNotFound && errors.Is(err, ErrWebhookNotFound) {
					t.Fatal("later-page 404 must not look like a missing repository webhook list")
				}
			}
		})
	}
}

func TestReconcilerRejectsUntrustworthyPagination(t *testing.T) {
	for _, target := range []string{"https://other.example/hooks?page=2", "/repos/owner/other/hooks?page=2", "/repos/owner/repo/hooks", "not-a-link"} {
		t.Run(target, func(t *testing.T) {
			var creates int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					atomic.AddInt32(&creates, 1)
					fmt.Fprint(w, `{"id":999}`)
					return
				}
				link := fmt.Sprintf(`<%s>; rel="next"`, target)
				if target == "not-a-link" {
					link = target
				}
				w.Header().Set("Link", link)
				fmt.Fprint(w, `[]`)
			}))
			defer server.Close()
			client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
			_, err := NewReconciler(client, nil).Reconcile(context.Background(), "owner", "repo", &Webhook{Config: WebhookConfig{URL: "https://example.com/desired"}})
			if err == nil || atomic.LoadInt32(&creates) != 0 {
				t.Fatalf("untrustworthy list caused success or creation: error=%v creates=%d", err, creates)
			}
		})
	}
}

func TestListWebhooksPreservesEnterprisePrefix(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&requests, 1)
		if req.URL.Path != "/api/v3/repos/owner/repo/hooks" {
			t.Errorf("unexpected path: %s", req.URL.Path)
		}
		if req.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"id":42}]`)
			return
		}
		w.Header().Set("Link", `<?page=2>; rel="next"`)
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer server.Close()
	client := NewHTTPGitHubClient(server.URL+"/api/v3", "test-token", server.Client())
	hooks, err := client.ListWebhooks(context.Background(), "owner", "repo")
	if err != nil || len(hooks) != 2 || hooks[1].ID != 42 || requests != 2 {
		t.Fatalf("unexpected result: hooks=%v error=%v requests=%d", hooks, err, requests)
	}
}
