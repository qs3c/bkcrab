package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type OpenAI struct{}

func (OpenAI) Category() string { return Category }
func (OpenAI) Name() string     { return "openai" }

func (o *OpenAI) Capability(model string) domain.Capability {
	maxImages := 4
	if model == "dall-e-3" {
		maxImages = 1
	}
	return domain.Capability{MaxImagesPerCall: maxImages, SupportedSizes: canonicalSizes()}
}

func (o *OpenAI) Generate(ctx context.Context, cfg domain.ResolvedProviderConfig, req domain.GenerateRequest) (domain.ProviderResult, error) {
	if err := validateAdapterRequest(o.Name(), req); err != nil {
		return domain.ProviderResult{}, err
	}
	if cfg.APIKey == "" {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorAuthConfig, o.Name(), 0, errors.New("missing credential"))
	}
	model := cfgModel(cfg, "gpt-image-1")
	body := map[string]any{"model": model, "prompt": req.Prompt, "n": req.Count, "size": openAISize(req.Size)}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/images/generations"
	}
	response, err := postJSON(ctx, endpoint, body, map[string]string{"Authorization": "Bearer " + cfg.APIKey})
	if err != nil {
		return domain.ProviderResult{}, typedTransportError(o.Name(), err)
	}
	defer response.Body.Close()
	if err := classifyHTTPResponse(o.Name(), response, http.StatusOK); err != nil {
		return domain.ProviderResult{}, err
	}
	var payload struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, o.Name(), response.StatusCode, err)
	}
	if len(payload.Data) == 0 {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorEmptyResult, o.Name(), response.StatusCode, errors.New("empty data"))
	}
	if len(payload.Data) != req.Count {
		return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorIncompleteResult, o.Name(), response.StatusCode, errors.New("unexpected image count"))
	}
	result := domain.ProviderResult{Provider: o.Name(), Model: model, Images: make([]domain.GeneratedImage, 0, len(payload.Data))}
	for _, item := range payload.Data {
		switch {
		case item.URL != "":
			if !validSourceURL(item.URL) {
				return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, o.Name(), response.StatusCode, errors.New("invalid source URL"))
			}
			result.Images = append(result.Images, domain.GeneratedImage{SourceURL: item.URL})
		case item.B64JSON != "":
			data, err := base64.StdEncoding.DecodeString(item.B64JSON)
			if err != nil {
				return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, o.Name(), response.StatusCode, err)
			}
			mimeType, ok := imageMIME(data)
			if !ok {
				return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, o.Name(), response.StatusCode, errors.New("decoded payload is not an image"))
			}
			result.Images = append(result.Images, domain.GeneratedImage{Bytes: data, MIMEType: mimeType})
		default:
			return domain.ProviderResult{}, domain.NewProviderError(domain.ErrorMalformedResult, o.Name(), response.StatusCode, errors.New("image has no source"))
		}
	}
	return result, nil
}

func (o *OpenAI) Execute(ctx context.Context, request toolproviders.Request) (toolproviders.Response, error) {
	parsed, err := parseArgs(request.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	result, err := o.Generate(ctx, domain.ResolvedProviderConfig{
		APIKey: request.Config.APIKey, Endpoint: request.Config.Endpoint,
		Options: request.Config.Options, Model: request.Config.Model,
	}, domain.GenerateRequest{Prompt: parsed.Prompt, Size: canonicalLegacySize(parsed.Size), Count: parsed.N})
	if err != nil {
		return toolproviders.Response{}, legacyProviderError(err)
	}
	return legacyProviderResponse(parsed.Prompt, result), nil
}

func openAISize(size string) string {
	switch size {
	case "", domain.SizeSquare:
		return "1024x1024"
	case domain.SizeLandscape:
		return "1536x1024"
	case domain.SizePortrait:
		return "1024x1536"
	default:
		return size
	}
}
