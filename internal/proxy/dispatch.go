package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"router/internal/jsonutil"
	"router/internal/openai"
	"router/internal/upstream"
)

// dispatch sends one attempt to up and returns the raw upstream HTTP
// response (or a transport-level error, e.g. DNS/TLS/connection reset).
// The caller owns closing resp.Body.
func dispatch(ctx context.Context, up *upstream.Upstream, clientReq *jsonutil.OrderedMap) (*http.Response, error) {
	outReq := openai.BuildUpstreamRequest(clientReq, up.Cfg)
	body, err := outReq.MarshalJSON()
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(up.Cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := up.Cfg.APIKey(); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	return up.Client.Do(httpReq)
}
