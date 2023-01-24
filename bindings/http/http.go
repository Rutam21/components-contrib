/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mitchellh/mapstructure"

	"github.com/dapr/components-contrib/bindings"
	"github.com/dapr/components-contrib/internal/utils"
	"github.com/dapr/kit/logger"
)

const (
	MTLSEnable     = "MTLSEnable"
	MTLSRootCA     = "MTLSRootCA"
	MTLSClientCert = "MTLSClientCert"
	MTLSClientKey  = "MTLSClientKey"

	TraceparentHeaderKey = "traceparent"
	TracestateHeaderKey  = "tracestate"
	TraceMetadataKey     = "traceHeaders"
)

// HTTPSource is a binding for an http url endpoint invocation
//
//revive:disable-next-line
type HTTPSource struct {
	metadata      httpMetadata
	client        *http.Client
	errorIfNot2XX bool
	logger        logger.Logger
}

type httpMetadata struct {
	URL            string `mapstructure:"url"`
	MTLSClientCert string `mapstructure:"mtlsClientCert"`
	MTLSClientKey  string `mapstructure:"mtlsClientKey"`
	MTLSRootCA     string `mapstructure:"mtlsRootCA"`
}

// NewHTTP returns a new HTTPSource.
func NewHTTP(logger logger.Logger) bindings.OutputBinding {
	return &HTTPSource{logger: logger}
}

// Init performs metadata parsing.
func (h *HTTPSource) Init(metadata bindings.Metadata) error {
	var err error
	if err = mapstructure.Decode(metadata.Properties, &h.metadata); err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if h.metadata.MTLSClientCert != "" && h.metadata.MTLSClientKey != "" {
		tlsConfig, err = h.readMTLSCertificates()
		if err != nil {
			return err
		}
	}

	// See guidance on proper HTTP client settings here:
	// https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}
	netTransport := &http.Transport{
		Dial:                dialer.Dial,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	if tlsConfig != nil && len(tlsConfig.Certificates) > 0 && tlsConfig.RootCAs != nil {
		netTransport.TLSClientConfig = tlsConfig
	}
	h.client = &http.Client{
		Timeout:   time.Second * 30,
		Transport: netTransport,
	}

	if val, ok := metadata.Properties["errorIfNot2XX"]; ok {
		h.errorIfNot2XX = utils.IsTruthy(val)
	} else {
		// Default behavior
		h.errorIfNot2XX = true
	}

	return nil
}

// readMTLSCertificates reads the certificates and key from the metadata and returns a tls.Config.
func (h *HTTPSource) readMTLSCertificates() (*tls.Config, error) {
	clientCertBytes, err := h.getPemBytes(MTLSClientCert, h.metadata.MTLSClientCert)
	if err != nil {
		return nil, err
	}
	clientKeyBytes, err := h.getPemBytes(MTLSClientKey, h.metadata.MTLSClientKey)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(clientCertBytes, clientKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if h.metadata.MTLSRootCA != "" {
		caCertBytes, err := h.getPemBytes(MTLSRootCA, h.metadata.MTLSRootCA)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		ok := caCertPool.AppendCertsFromPEM(caCertBytes)
		if !ok {
			return nil, errors.New("failed to add root certificate to certpool")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

// getPemBytes returns the PEM encoded bytes from the provided certName and certData.
// If the certData is a file path, it reads the file and returns the bytes.
// Else if the certData is a PEM encoded string, it returns the bytes.
// Else it returns an error.
func (h *HTTPSource) getPemBytes(certName, certData string) ([]byte, error) {
	// Read the file
	pemBytes, err := os.ReadFile(certData)
	// If there is an error assume it is already PEM encoded string not a file path.
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to read %q file: %w", certName, err)
		}
		if !isValidPEM(certData) {
			return nil, fmt.Errorf("provided %q value is neither a valid file path or nor a valid pem encoded string", certName)
		}
		return []byte(certData), nil
	}
	return pemBytes, nil
}

// isValidPEM validates the provided input has PEM formatted block.
func isValidPEM(val string) bool {
	block, _ := pem.Decode([]byte(val))
	return block != nil
}

// Operations returns the supported operations for this binding.
func (h *HTTPSource) Operations() []bindings.OperationKind {
	return []bindings.OperationKind{
		bindings.CreateOperation, // For backward compatibility
		"get",
		"head",
		"post",
		"put",
		"patch",
		"delete",
		"options",
		"trace",
	}
}

// Invoke performs an HTTP request to the configured HTTP endpoint.
func (h *HTTPSource) Invoke(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	u := h.metadata.URL

	errorIfNot2XX := h.errorIfNot2XX // Default to the component config (default is true)

	if req.Metadata != nil {
		if path, ok := req.Metadata["path"]; ok {
			// Simplicity and no "../../.." type exploits.
			u = fmt.Sprintf("%s/%s", strings.TrimRight(u, "/"), strings.TrimLeft(path, "/"))
			if strings.Contains(u, "..") {
				return nil, fmt.Errorf("invalid path: %s", path)
			}
		}

		if _, ok := req.Metadata["errorIfNot2XX"]; ok {
			errorIfNot2XX = utils.IsTruthy(req.Metadata["errorIfNot2XX"])
		}
	} else {
		// Prevent things below from failing if req.Metadata is nil.
		req.Metadata = make(map[string]string)
	}

	var body io.Reader
	method := strings.ToUpper(string(req.Operation))
	// For backward compatibility
	if method == "CREATE" {
		method = "POST"
	}
	switch method {
	case "PUT", "POST", "PATCH":
		body = bytes.NewBuffer(req.Data)
	case "GET", "HEAD", "DELETE", "OPTIONS", "TRACE":
	default:
		return nil, fmt.Errorf("invalid operation: %s", req.Operation)
	}

	request, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}

	// Set default values for Content-Type and Accept headers.
	if body != nil {
		if _, ok := req.Metadata["Content-Type"]; !ok {
			request.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
	}
	if _, ok := req.Metadata["Accept"]; !ok {
		request.Header.Set("Accept", "application/json; charset=utf-8")
	}

	// Any metadata keys that start with a capital letter
	// are treated as request headers
	for mdKey, mdValue := range req.Metadata {
		keyAsRunes := []rune(mdKey)
		if len(keyAsRunes) > 0 && unicode.IsUpper(keyAsRunes[0]) {
			request.Header.Set(mdKey, mdValue)
		}
	}

	// HTTP binding needs to inject traceparent header for proper tracing stack.
	if tp, ok := req.Metadata[TraceparentHeaderKey]; ok && tp != "" {
		if _, ok := request.Header[http.CanonicalHeaderKey(TraceparentHeaderKey)]; ok {
			h.logger.Warn("tracing enabled, overwriting Traceparent in request headers")
		}

		request.Header.Set(TraceparentHeaderKey, tp)
	}
	if ts, ok := req.Metadata[TracestateHeaderKey]; ok && ts != "" {
		if _, ok := request.Header[http.CanonicalHeaderKey(TracestateHeaderKey)]; ok {
			h.logger.Warn("tracing enabled, overwriting Tracestate in request headers")
		}

		request.Header.Set(TracestateHeaderKey, ts)
	}

	// Send the question
	resp, err := h.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the response body. For empty responses (e.g. 204 No Content)
	// `b` will be an empty slice.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	metadata := make(map[string]string, len(resp.Header)+2)
	// Include status code & desc
	metadata["statusCode"] = strconv.Itoa(resp.StatusCode)
	metadata["status"] = resp.Status

	// Response headers are mapped from `map[string][]string` to `map[string]string`
	// where headers with multiple values are delimited with ", ".
	for key, values := range resp.Header {
		metadata[key] = strings.Join(values, ", ")
	}

	// Create an error for non-200 status codes unless suppressed.
	if errorIfNot2XX && resp.StatusCode/100 != 2 {
		err = fmt.Errorf("received status code %d", resp.StatusCode)
	}

	return &bindings.InvokeResponse{
		Data:     b,
		Metadata: metadata,
	}, err
}
