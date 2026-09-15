package devbridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"
)

const (
	DefaultAPIBaseURL  = "https://bridge.developer.myhuaweicloud.com/open-api-inner/v1/relay-controller"
	DefaultGatewayAddr = "gateway.devbridge-s2.hwtunnel.com:443"
	DefaultGatewayHost = "devbridge-s2.hwtunnel.com"
	DefaultClusterID   = "devbridge-s2"
)

var (
	tunnelIDRegexp   = regexp.MustCompile(`^[a-z2-7]{8}$`)
	tunnelNameRegexp = regexp.MustCompile(`^[\x{4e00}-\x{9fa5}A-Za-z0-9]([\x{4e00}-\x{9fa5}A-Za-z0-9-]{0,62}[\x{4e00}-\x{9fa5}A-Za-z0-9])?$`)
	tunnelDescRegexp = regexp.MustCompile(`^[\x{4e00}-\x{9fa5}A-Za-z0-9]{0,64}$`)
)

// AllPortsSentinel 表示"所有端口"的哨兵值，对应后端存储的 -1。
// 它只对 visitor URL 访问有意义（网关按 SNI 动态路由任意端口），
// host/connect 的 SSH 主动端口转发不应转发该值。
const AllPortsSentinel = -1

// Config holds the SDK client configuration. A zero Config is valid —
// missing fields fall back to environment variables and sensible defaults.
type Config struct {
	// APIKey authenticates REST API requests (X-API-Key header).
	// Defaults to HW_API_KEY environment variable.
	APIKey string

	// APIBaseURL is the REST API base URL.
	// Defaults to DefaultAPIBaseURL.
	APIBaseURL string

	// GatewayAddr is the WebSocket gateway address (host:port).
	// Defaults to DefaultGatewayAddr.
	GatewayAddr string

	// GatewayHost is the WebSocket gateway SNI host.
	// Defaults to DefaultGatewayHost.
	GatewayHost string

	// HTTPClient optionally overrides the HTTP client.
	// If nil, a default client with 30s timeout is used.
	HTTPClient *http.Client

	// StatusWriter receives user-facing status lines (connection progress,
	// hosted ports, forwarding info). Defaults to os.Stdout; set it to
	// io.Discard to silence these outputs.
	StatusWriter io.Writer
}

// resolve returns a copy with defaults and env-var fallbacks applied.
func (cfg Config) resolve() Config {
	out := cfg
	if out.APIBaseURL == "" {
		out.APIBaseURL = DefaultAPIBaseURL
	}
	if out.GatewayAddr == "" {
		out.GatewayAddr = DefaultGatewayAddr
	}
	if out.GatewayHost == "" {
		out.GatewayHost = DefaultGatewayHost
	}
	if out.APIKey == "" {
		out.APIKey = os.Getenv("HW_API_KEY")
	}
	if out.StatusWriter == nil {
		out.StatusWriter = os.Stdout
	}
	if out.HTTPClient == nil {
		out.HTTPClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	return out
}

// Client DevBridge SDK 客户端
type Client struct {
	apiKey       string
	apiBaseURL   string
	gatewayAddr  string
	gatewayHost  string
	httpClient   *http.Client
	logger       *slog.Logger
	statusWriter io.Writer
}

// NewClient creates a new SDK client from the given Config.
// A zero Config is valid; APIKey falls back to HW_API_KEY env var,
// and other fields fall back to sensible defaults.
func NewClient(cfg Config) (*Client, error) {
	resolved := cfg.resolve()
	return &Client{
		apiKey:       resolved.APIKey,
		apiBaseURL:   resolved.APIBaseURL,
		gatewayAddr:  resolved.GatewayAddr,
		gatewayHost:  resolved.GatewayHost,
		httpClient:   resolved.HTTPClient,
		logger:       slog.Default(),
		statusWriter: resolved.StatusWriter,
	}, nil
}

const (
	headerXAPIKey     = "X-API-Key"
	headerContentType = "content-type"
	headerJSON        = "application/json"
)

func (c *Client) resolveAPIKey() (string, error) {
	if c.apiKey == "" {
		return "", ErrMissingAPIKey
	}
	return c.apiKey, nil
}

func (c *Client) isDebugEnabled() bool {
	return c.logger.Enabled(context.Background(), slog.LevelDebug)
}

func (c *Client) logHTTPRequest(req *http.Request, body []byte) {
	if !c.isDebugEnabled() {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", req.Method),
		slog.String("url", req.URL.String()),
	}
	if len(body) > 0 {
		attrs = append(attrs, slog.String("body", string(body)))
	}
	c.logger.LogAttrs(context.Background(), slog.LevelDebug, "HTTP request", attrs...)
}

func (c *Client) logHTTPResponse(resp *http.Response, body []byte, elapsed time.Duration) {
	if !c.isDebugEnabled() {
		return
	}
	attrs := []slog.Attr{
		slog.Int("statusCode", resp.StatusCode),
		slog.String("status", resp.Status),
		slog.Int64("elapsed", elapsed.Milliseconds()),
	}
	c.logger.LogAttrs(context.Background(), slog.LevelDebug, "HTTP response", attrs...)
	if len(body) > 0 {
		c.logger.LogAttrs(context.Background(), slog.LevelDebug, "HTTP response body",
			slog.String("data", string(body)),
			slog.String("size", fmt.Sprintf("%d bytes total", len(body))),
		)
	}
}

func (c *Client) statusf(format string, args ...any) {
	fmt.Fprintf(c.statusWriter, format, args...)
}

func (c *Client) statusln(args ...any) {
	fmt.Fprintln(c.statusWriter, args...)
}

func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	apiKey, err := c.resolveAPIKey()
	if err != nil {
		return err
	}

	url := c.apiBaseURL + path

	var bodyBytes []byte
	hasBody := method == http.MethodPost || method == http.MethodPut
	if hasBody && body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set(headerXAPIKey, apiKey)
	if hasBody {
		req.Header.Set(headerContentType, headerJSON)
	}

	c.logHTTPRequest(req, bodyBytes)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	c.logHTTPResponse(resp, respBody, time.Since(start))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if apiErr := parseAPIError(respBody); apiErr != nil {
			return apiErr
		}
		return fmt.Errorf("server error: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// parseAPIError 尝试从响应体解析 {error: {code, message}} 格式的业务错误。
func parseAPIError(body []byte) *APIError {
	var eb errorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error.Code != "" {
		return &APIError{Code: eb.Error.Code, Message: eb.Error.Message}
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	return c.doRequest(ctx, http.MethodGet, path, nil, result)
}

func (c *Client) post(ctx context.Context, path string, body, result any) error {
	return c.doRequest(ctx, http.MethodPost, path, body, result)
}

func (c *Client) put(ctx context.Context, path string, body, result any) error {
	return c.doRequest(ctx, http.MethodPut, path, body, result)
}

func (c *Client) delete(ctx context.Context, path string, result any) error {
	return c.doRequest(ctx, http.MethodDelete, path, nil, result)
}

func validateTunnelID(id string) error {
	if !tunnelIDRegexp.MatchString(id) {
		return fmt.Errorf("%w: got %q", ErrInvalidTunnelID, id)
	}
	return nil
}

func validateTunnelDescription(description string) error {
	if !tunnelDescRegexp.MatchString(description) {
		return fmt.Errorf("%w: got %q", ErrInvalidTunnelDescription, description)
	}
	return nil
}

func validatePortNumber(port int) error {
	if port == AllPortsSentinel {
		return nil
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: got %d", ErrInvalidPort, port)
	}
	return nil
}

// filterForwardPorts 过滤掉"所有端口"哨兵值，返回仅包含真实端口（1-65535）的列表。
// host/connect 的 SSH 端口转发不应转发 -1（哨兵值对应 uint32 的 4294967295，
// 不是合法监听端口），它只对 visitor URL 访问有意义。
func filterForwardPorts(ports []int) []int {
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p == AllPortsSentinel {
			continue
		}
		out = append(out, p)
	}
	return out
}

func validateProtocol(protocol string) error {
	switch protocol {
	case "http", "https", "auto", "":
		return nil
	default:
		return fmt.Errorf("%w: got %s", ErrInvalidProtocol, protocol)
	}
}

func validateScope(scope string) error {
	if scope != "host" && scope != "connect" {
		return fmt.Errorf("%w: got %s", ErrInvalidScope, scope)
	}
	return nil
}
