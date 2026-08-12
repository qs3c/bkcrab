package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type Replicate struct{}

func (Replicate) Category() string { return Category }
func (Replicate) Name() string     { return "replicate" }

var replicateModelRoutes = map[string]string{
	"flux-schnell": "black-forest-labs/flux-schnell", "flux-dev": "black-forest-labs/flux-dev",
	"flux-pro": "black-forest-labs/flux-1.1-pro", "sdxl": "stability-ai/sdxl", "ideogram": "ideogram-ai/ideogram-v2",
}

func (r *Replicate) Capability(string) domain.Capability {
	return domain.Capability{MaxImagesPerCall: 4, SupportedSizes: canonicalSizes()}
}

func (r *Replicate) Generate(ctx context.Context, cfg domain.ResolvedProviderConfig, req domain.GenerateRequest) (domain.ProviderResult, error) {
	if err := validateAdapterRequest(r.Name(), req); err != nil {
		return domain.ProviderResult{}, err
	}
	if cfg.APIKey == "" {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorAuthConfig, r.Name(), 0, errors.New("missing credential"))
	}
	model := cfgModel(cfg, "flux-schnell")
	path := replicateModelRoutes[model]
	if path == "" {
		path = model
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.replicate.com/v1/models/" + strings.Trim(path, "/") + "/predictions"
	}
	body := map[string]any{"input": map[string]any{
		"prompt": req.Prompt, "num_outputs": req.Count, "aspect_ratio": replicateSize(req.Size),
	}}
	response, err := postJSON(ctx, endpoint, body, map[string]string{
		"Authorization": "Bearer " + cfg.APIKey, "Prefer": "wait",
	})
	if err != nil {
		return domain.ProviderResult{}, typedTransportError(r.Name(), err)
	}
	defer response.Body.Close()
	if err := classifyHTTPResponse(r.Name(), response, http.StatusOK, http.StatusCreated); err != nil {
		return domain.ProviderResult{}, err
	}
	var payload struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, r.Name(), response.StatusCode, err)
	}
	if payload.Status == "failed" {
		kind := domain.ErrorUpstreamTransient
		lower := strings.ToLower(payload.Error)
		if strings.Contains(lower, "safety") || strings.Contains(lower, "content policy") {
			kind = domain.ErrorSafetyRejected
		}
		return domain.ProviderResult{}, domain.NewProviderError(kind, r.Name(), response.StatusCode, errors.New("prediction failed"))
	}
	if payload.Status != "succeeded" {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorUpstreamTransient, r.Name(), response.StatusCode, errors.New("prediction incomplete"))
	}
	urls, valid := decodeReplicateOutput(payload.Output)
	if !valid {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, r.Name(), response.StatusCode, errors.New("invalid output shape"))
	}
	if len(urls) == 0 {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorEmptyResult, r.Name(), response.StatusCode, errors.New("empty output"))
	}
	if len(urls) != req.Count {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorIncompleteResult, r.Name(), response.StatusCode, errors.New("unexpected image count"))
	}
	result := domain.ProviderResult{Provider: r.Name(), Model: model, Images: make([]domain.GeneratedImage, 0, len(urls))}
	for _, sourceURL := range urls {
		if !validSourceURL(sourceURL) {
			return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, r.Name(), response.StatusCode, errors.New("invalid source URL"))
		}
		result.Images = append(result.Images, domain.GeneratedImage{SourceURL: sourceURL})
	}
	return result, nil
}

func (r *Replicate) Execute(ctx context.Context, request toolproviders.Request) (toolproviders.Response, error) {
	parsed, err := parseArgs(request.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	result, err := r.Generate(ctx, domain.ResolvedProviderConfig{
		APIKey: request.Config.APIKey, Endpoint: request.Config.Endpoint, Options: request.Config.Options, Model: request.Config.Model,
	}, domain.GenerateRequest{Prompt: parsed.Prompt, Size: canonicalLegacySize(parsed.Size), Count: parsed.N})
	if err != nil {
		return toolproviders.Response{}, legacyProviderError(err)
	}
	return legacyProviderResponse(parsed.Prompt, result), nil
}

func replicateSize(size string) string {
	switch size {
	case "", domain.SizeSquare:
		return "1:1"
	case domain.SizeLandscape:
		return "4:3"
	case domain.SizePortrait:
		return "3:4"
	default:
		return size
	}
}

func decodeReplicateOutput(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, true
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, true
		}
		return []string{one}, true
	}
	return nil, false
}
