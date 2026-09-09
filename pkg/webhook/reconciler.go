package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrWebhookNotFound indicates that the requested webhook does not exist (HTTP 404).
	ErrWebhookNotFound = errors.New("webhook not found")
	// ErrUnauthorized indicates invalid authentication credentials (HTTP 401).
	ErrUnauthorized = errors.New("github api unauthorized: check token/credentials")
	// ErrForbidden indicates insufficient permissions (HTTP 403).
	ErrForbidden = errors.New("github api forbidden: check repository permissions")
	// ErrRateLimited indicates GitHub API rate limit was exceeded (HTTP 429).
	ErrRateLimited = errors.New("github api rate limited")
	// ErrServerError indicates a GitHub server-side failure (HTTP 5xx).
	ErrServerError = errors.New("github api server error")
)

// WebhookConfig represents the configuration payload for a GitHub webhook.
type WebhookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
	Secret      string `json:"secret,omitempty"`
	InsecureSSL string `json:"insecure_ssl,omitempty"`
}

// Webhook represents a GitHub repository webhook.
type Webhook struct {
	ID     int64         `json:"id,omitempty"`
	Name   string        `json:"name,omitempty"`
	Active bool          `json:"active"`
	Events []string      `json:"events,omitempty"`
	Config WebhookConfig `json:"config"`
}

// GitHubClient defines the client contract for GitHub webhook operations.
type GitHubClient interface {
	GetWebhook(ctx context.Context, owner, repo string, hookID int64) (*Webhook, error)
	ListWebhooks(ctx context.Context, owner, repo string) ([]*Webhook, error)
	CreateWebhook(ctx context.Context, owner, repo string, hook *Webhook) (*Webhook, error)
}

// HTTPGitHubClient implements GitHubClient using standard HTTP requests.
type HTTPGitHubClient struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewHTTPGitHubClient creates a new HTTP-based GitHub API client.
func NewHTTPGitHubClient(baseURL, token string, httpClient *http.Client) *HTTPGitHubClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &HTTPGitHubClient{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: httpClient,
	}
}

func (c *HTTPGitHubClient) newRequest(ctx context.Context, method, path string, body interface{}) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	reqURL := fmt.Sprintf("%s%s", c.BaseURL, path)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *HTTPGitHubClient) checkResponseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrWebhookNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: status %d body: %s", ErrUnauthorized, resp.StatusCode, string(body))
	case http.StatusForbidden:
		return fmt.Errorf("%w: status %d body: %s", ErrForbidden, resp.StatusCode, string(body))
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d body: %s", ErrRateLimited, resp.StatusCode, string(body))
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Errorf("%w: status %d body: %s", ErrServerError, resp.StatusCode, string(body))
	default:
		return fmt.Errorf("github api error: status %d body: %s", resp.StatusCode, string(body))
	}
}

// GetWebhook retrieves a specific webhook by ID.
func (c *HTTPGitHubClient) GetWebhook(ctx context.Context, owner, repo string, hookID int64) (*Webhook, error) {
	path := fmt.Sprintf("/repos/%s/%s/hooks/%d", owner, repo, hookID)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error querying github webhook: %w", err)
	}
	defer resp.Body.Close()

	if err := c.checkResponseError(resp); err != nil {
		return nil, err
	}

	var hook Webhook
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		return nil, fmt.Errorf("failed to decode webhook response: %w", err)
	}
	return &hook, nil
}

// ListWebhooks retrieves all webhooks for a repository.
func (c *HTTPGitHubClient) ListWebhooks(ctx context.Context, owner, repo string) ([]*Webhook, error) {
	basePath := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)
	nextPath := basePath
	seen := make(map[string]bool)
	var hooks []*Webhook
	for page := 1; nextPath != ""; page++ {
		if seen[nextPath] || page > 1000 {
			return nil, fmt.Errorf("incomplete webhook listing: pagination cycle or page limit")
		}
		seen[nextPath] = true
		req, err := c.newRequest(ctx, http.MethodGet, nextPath, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("network error listing github webhooks: %w", err)
		}
		if err := c.checkResponseError(resp); err != nil {
			resp.Body.Close()
			if page > 1 && errors.Is(err, ErrWebhookNotFound) {
				// A missing later page is an incomplete list, not evidence that
				// the repository has no webhook. Do not expose the create sentinel.
				return nil, fmt.Errorf("incomplete webhook listing on page %d: %v", page, err)
			}
			return nil, err
		}
		var batch []*Webhook
		err = json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to decode webhooks response: %w", err)
		}
		hooks = append(hooks, batch...)
		nextPath, err = nextWebhookListPath(resp.Header.Get("Link"), req.URL, basePath)
		if err != nil {
			return nil, err
		}
	}
	return hooks, nil
}

// Only copy pagination query parameters back to the original endpoint. Never
// send the client's authorization token to a different host or repository.
func nextWebhookListPath(header string, current *url.URL, basePath string) (string, error) {
	if strings.TrimSpace(header) == "" {
		return "", nil
	}
	for _, link := range strings.Split(header, ",") {
		start, end := strings.Index(link, "<"), strings.Index(link, ">")
		if start < 0 || end <= start {
			return "", fmt.Errorf("incomplete webhook listing: malformed pagination link")
		}
		isNext := false
		for _, parameter := range strings.Split(link[end+1:], ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && name == "rel" {
				for _, relation := range strings.Fields(strings.Trim(value, "\"")) {
					isNext = isNext || relation == "next"
				}
			}
		}
		if !isNext {
			continue
		}
		target, err := url.Parse(link[start+1 : end])
		if err != nil {
			return "", fmt.Errorf("incomplete webhook listing: invalid pagination URL: %w", err)
		}
		target = current.ResolveReference(target)
		if target.Scheme != current.Scheme || target.Host != current.Host || target.Path != current.Path || target.User != nil {
			return "", fmt.Errorf("incomplete webhook listing: pagination target differs from original endpoint")
		}
		query := target.Query().Encode()
		if query == "" {
			return basePath, nil
		}
		return basePath + "?" + query, nil
	}
	return "", nil
}

// CreateWebhook creates a new webhook for a repository.
func (c *HTTPGitHubClient) CreateWebhook(ctx context.Context, owner, repo string, hook *Webhook) (*Webhook, error) {
	path := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)
	req, err := c.newRequest(ctx, http.MethodPost, path, hook)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error creating github webhook: %w", err)
	}
	defer resp.Body.Close()

	if err := c.checkResponseError(resp); err != nil {
		return nil, err
	}

	var created Webhook
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("failed to decode created webhook response: %w", err)
	}
	return &created, nil
}

// Reconciler manages the lifecycle and reconciliation of GitHub webhooks.
type Reconciler struct {
	client GitHubClient
	logger *log.Logger
}

// NewReconciler creates a new webhook reconciler.
func NewReconciler(client GitHubClient, logger *log.Logger) *Reconciler {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Reconciler{
		client: client,
		logger: logger,
	}
}

// Reconcile ensures the desired webhook exists on the target repository.
// If the webhook retrieval indicates 404 (ErrWebhookNotFound), it creates the webhook.
// Non-404 errors (5xx, 429, 401, 403, network timeouts) are propagated immediately
// without attempting creation to prevent duplicate webhooks and masking critical errors.
func (r *Reconciler) Reconcile(ctx context.Context, owner, repo string, desired *Webhook) (*Webhook, error) {
	hooks, err := r.client.ListWebhooks(ctx, owner, repo)
	if err != nil {
		if errors.Is(err, ErrWebhookNotFound) {
			r.logger.Printf("Webhooks endpoint returned 404 for %s/%s; creating new webhook", owner, repo)
			return r.client.CreateWebhook(ctx, owner, repo, desired)
		}

		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
			r.logger.Printf("Authentication/Authorization failure for %s/%s: %v", owner, repo, err)
			return nil, fmt.Errorf("permission error reconciling webhook for %s/%s: %w", owner, repo, err)
		}

		if errors.Is(err, ErrRateLimited) {
			r.logger.Printf("Rate limit encountered while reconciling %s/%s: %v", owner, repo, err)
			return nil, fmt.Errorf("rate limit exceeded reconciling webhook for %s/%s: %w", owner, repo, err)
		}

		if errors.Is(err, ErrServerError) {
			r.logger.Printf("GitHub server error encountered while reconciling %s/%s: %v", owner, repo, err)
			return nil, fmt.Errorf("github server error reconciling webhook for %s/%s: %w", owner, repo, err)
		}

		r.logger.Printf("Error fetching webhooks for %s/%s: %v", owner, repo, err)
		return nil, fmt.Errorf("failed to list webhooks for %s/%s: %w", owner, repo, err)
	}

	// Look for an existing webhook with the matching URL
	for _, h := range hooks {
		if h.Config.URL == desired.Config.URL {
			r.logger.Printf("Webhook already exists for %s/%s (ID: %d)", owner, repo, h.ID)
			return h, nil
		}
	}

	// Webhook does not exist in the list; safely create it
	r.logger.Printf("Webhook not found in list for %s/%s; creating new webhook", owner, repo)
	return r.client.CreateWebhook(ctx, owner, repo, desired)
}
