package link

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// TLSOptions are the knobs an agent exposes for reaching its controller over
// https: a self-signed certificate (Insecure) or a private CA (CAFile).
type TLSOptions struct {
	Insecure bool
	CAFile   string
}

// NewHTTPClient builds the client for joins, announces and the WebSocket
// handshake. HTTP/1.1 only: an HTTP/2 connection cannot be upgraded.
func NewHTTPClient(o TLSOptions) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.Insecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // opt-in by the operator
	}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no PEM certificates", o.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{Transport: tr}, nil
}

// Endpoint joins the controller's base URL and an API path.
func Endpoint(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// WebSocketURL maps the controller's base URL plus an API path to the ws:// or
// wss:// URL of that endpoint.
func WebSocketURL(base, path string) (string, error) {
	u, err := url.Parse(Endpoint(base, path))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}
