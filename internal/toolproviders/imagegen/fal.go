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

type Fal struct{}

func (Fal) Category() string { return Category }
func (Fal) Name() string     { return "fal" }

var falModelRoutes = map[string]string{
	"flux-dev": "fal-ai/flux/dev", "flux-schnell": "fal-ai/flux/schnell", "flux-pro": "fal-ai/flux-pro",
}

func (f *Fal) Capability(string) domain.Capability {
	return domain.Capability{MaxImagesPerCall: 4, SupportedSizes: canonicalSizes()}
}

func (f *Fal) Generate(ctx context.Context, cfg domain.ResolvedProviderConfig, req domain.GenerateRequest) (domain.ProviderResult, error) {
	if err := validateAdapterRequest(f.Name(), req); err != nil {
		return domain.ProviderResult{}, err
	}
	if cfg.APIKey == "" {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorAuthConfig, f.Name(), 0, errors.New("missing credential"))
	}
	model := cfgModel(cfg, "flux-dev")
	path := falModelRoutes[model]
	if path == "" {
		path = model
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://fal.run/" + strings.TrimPrefix(path, "/")
	}
	body := map[string]any{"prompt": req.Prompt, "num_images": req.Count, "image_size": falSize(req.Size)}
	response, err := postJSON(ctx, endpoint, body, map[string]string{"Authorization": "Key " + cfg.APIKey})
	if err != nil {
		return domain.ProviderResult{}, typedTransportError(f.Name(), err)
	}
	defer response.Body.Close()
	if err := classifyHTTPResponse(f.Name(), response, http.StatusOK); err != nil {
		return domain.ProviderResult{}, err
	}
	var payload struct {
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, f.Name(), response.StatusCode, err)
	}
	if len(payload.Images) == 0 {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorEmptyResult, f.Name(), response.StatusCode, errors.New("empty images"))
	}
	if len(payload.Images) != req.Count {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorIncompleteResult, f.Name(), response.StatusCode, errors.New("unexpected image count"))
	}
	result := domain.ProviderResult{Provider: f.Name(), Model: model, Images: make([]domain.GeneratedImage, 0, len(payload.Images))}
	for _, image := range payload.Images {
		if !validSourceURL(image.URL) {
			return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, f.Name(), response.StatusCode, errors.New("invalid source URL"))
		}
		result.Images = append(result.Images, domain.GeneratedImage{SourceURL: image.URL})
	}
	return result, nil
}

func (f *Fal) Execute(ctx context.Context, request toolproviders.Request) (toolproviders.Response, error) {
	parsed, err := parseArgs(request.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	result, err := f.Generate(ctx, domain.ResolvedProviderConfig{
		APIKey: request.Config.APIKey, Endpoint: request.Config.Endpoint, Options: request.Config.Options, Model: request.Config.Model,
	}, domain.GenerateRequest{Prompt: parsed.Prompt, Size: canonicalLegacySize(parsed.Size), Count: parsed.N})
	if err != nil {
		return toolproviders.Response{}, legacyProviderError(err)
	}
	return legacyProviderResponse(parsed.Prompt, result), nil
}

func falSize(size string) string {
	switch size {
	case "", domain.SizeSquare:
		return "square_hd"
	case domain.SizeLandscape:
		return "landscape_4_3"
	case domain.SizePortrait:
		return "portrait_4_3"
	default:
		return size
	}
}
