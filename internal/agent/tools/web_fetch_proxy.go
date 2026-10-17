package tools

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// webFetchClient honors the operator's device proxy. Direct connections retain
// address-pinned SSRF protection. A configured proxy is trusted to resolve public
// hostnames remotely, just as in the DSH HTTP fetch provider; explicit internal
// URLs are rejected before dispatch and again on every redirect.
func webFetchClient(req *http.Request) (*http.Client, error) {
	proxyURL, err := httpproxy.FromEnvironment().ProxyFunc()(req.URL)
	if err != nil {
		return nil, fmt.Errorf("web fetch proxy configuration: %w", err)
	}
	// Validate before ProxyFunc's implicit loopback bypass can select direct.
	if err := validateWebFetchProxyTarget(req.URL); err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return safeFetchClient, nil
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("web fetch proxy must use http or https")
	}
	transport := safeFetchClient.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	// This dial is to the trusted proxy, which can legitimately be loopback.
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: transport,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return validateWebFetchProxyTarget(next.URL)
		},
	}, nil
}

func validateWebFetchProxyTarget(target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("web fetch requires http or https")
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" || target.User != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("blocked web fetch target")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedAddr(ip) {
			return fmt.Errorf("blocked web fetch address")
		}
	} else {
		// Refuse noncanonical numeric IP forms (127.1, integer IPv4, zone IDs).
		last := host[strings.LastIndex(host, ".")+1:]
		if strings.Contains(host, "%") || (last[0] >= '0' && last[0] <= '9') {
			return fmt.Errorf("blocked ambiguous web fetch address")
		}
	}
	return nil
}
