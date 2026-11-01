package meter

import (
	"context"
	"io"
	"net/http"
	"time"
)

const catalogURL = "https://models.dev/api.json"
const maxCatalogBytes = 16 << 20

// A dedicated transport has no environment proxy, cookie jar, or host credentials.
func catalogClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 1, IdleConnTimeout: time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 << 10}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.Host != "models.dev" || req.URL.User != nil {
			return errCatalog
		}
		return nil
	}}
}
func fetchCatalog(ctx context.Context) ([]byte, error) {
	client := catalogClient()
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, errCatalog
	}
	req.Header.Set("Accept", "application/json")
	return downloadCatalog(client, req)
}
func downloadCatalog(client *http.Client, req *http.Request) ([]byte, error) {
	res, err := client.Do(req)
	if err != nil {
		return nil, errCatalog
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength > maxCatalogBytes {
		return nil, errCatalog
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCatalogBytes+1))
	if err != nil || len(b) > maxCatalogBytes {
		return nil, errCatalog
	}
	return b, nil
}
