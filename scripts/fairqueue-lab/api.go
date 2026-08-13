package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"
)

type apiClient struct {
	base  *url.URL
	http  *http.Client
	token string
}

type apiAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type apiKB struct {
	ID string `json:"id"`
}

type apiDocument struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Progress struct {
		Stage string `json:"stage"`
	} `json:"progress"`
}

func newAPIClient(rawBase, token string) (*apiClient, error) {
	base, err := url.Parse(strings.TrimRight(rawBase, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", rawBase)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &apiClient{base: base, token: token, http: &http.Client{Jar: jar, Timeout: 2 * time.Minute}}, nil
}

func (c *apiClient) endpoint(parts ...string) string {
	copy := *c.base
	joined := copy.Path
	for _, part := range parts {
		joined = path.Join(joined, part)
	}
	copy.Path = joined
	return copy.String()
}

func (c *apiClient) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string, output any, allowed ...int) error {
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(endpoint), body)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	ok := response.StatusCode >= 200 && response.StatusCode < 300
	if len(allowed) > 0 {
		ok = false
		for _, status := range allowed {
			ok = ok || response.StatusCode == status
		}
	}
	if !ok {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s %s returned %d: %s", method, endpoint, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, endpoint, err)
	}
	return nil
}

func (c *apiClient) json(ctx context.Context, method, endpoint string, input, output any, allowed ...int) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	return c.do(ctx, method, endpoint, body, "application/json", output, allowed...)
}

func (c *apiClient) login(ctx context.Context, login, password string) (apiAccount, error) {
	var response struct {
		User apiAccount `json:"user"`
	}
	err := c.json(ctx, http.MethodPost, "/api/login", map[string]string{"login": login, "password": password}, &response, http.StatusOK)
	return response.User, err
}

func (c *apiClient) createUser(ctx context.Context, username, email, password string) (apiAccount, error) {
	var response struct {
		User apiAccount `json:"user"`
	}
	err := c.json(ctx, http.MethodPost, "/api/users", map[string]any{
		"username": username, "email": email, "password": password,
		"displayName": "Fair queue lab " + username, "role": "user", "agentQuota": 0,
	}, &response, http.StatusCreated)
	return response.User, err
}

func (c *apiClient) deleteUser(ctx context.Context, id string) error {
	return c.json(ctx, http.MethodDelete, "/api/users/"+url.PathEscape(id), nil, nil, http.StatusOK)
}

func (c *apiClient) createKB(ctx context.Context, name, description string) (apiKB, error) {
	var response apiKB
	err := c.json(ctx, http.MethodPost, "/api/rag/kbs", map[string]any{
		"name": name, "description": description, "parseMode": "standard", "enrichmentEnabled": false,
	}, &response, http.StatusCreated)
	return response, err
}

func (c *apiClient) uploadDocument(ctx context.Context, kbID, name string, data []byte) (apiDocument, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return apiDocument{}, err
	}
	if _, err := part.Write(data); err != nil {
		return apiDocument{}, err
	}
	if err := writer.Close(); err != nil {
		return apiDocument{}, err
	}
	var response apiDocument
	err = c.do(ctx, http.MethodPost, "/api/rag/kbs/"+url.PathEscape(kbID)+"/documents", &body, writer.FormDataContentType(), &response, http.StatusAccepted)
	return response, err
}

func (c *apiClient) listDocuments(ctx context.Context, kbID string) ([]apiDocument, error) {
	var response []apiDocument
	err := c.json(ctx, http.MethodGet, "/api/rag/kbs/"+url.PathEscape(kbID)+"/documents", nil, &response, http.StatusOK)
	return response, err
}

func (c *apiClient) fairQueueHealth(ctx context.Context) (fairQueueHealth, error) {
	var response struct {
		FairQueue fairQueueHealth `json:"fairQueue"`
	}
	err := c.json(ctx, http.MethodGet, "/api/admin/health/fairqueue", nil, &response, http.StatusOK)
	return response.FairQueue, err
}
