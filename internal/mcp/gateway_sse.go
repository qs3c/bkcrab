package mcp

type GatewaySSEClient struct {
	*HTTPClient
}

func NewGatewaySSEClient(messageURL string, headers map[string]string) *GatewaySSEClient {
	return &GatewaySSEClient{HTTPClient: NewHTTPClient(messageURL, headers)}
}
