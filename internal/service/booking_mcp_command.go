package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	bookingMCPProtocolVersion        = "2025-06-18"
	bookingMCPServerName             = "longtradego-booking-mcp"
	bookingMCPToolCatalogQuery       = "booking_catalog_query"
	bookingMCPToolReservationsList   = "booking_reservations_list"
	bookingMCPToolIntentParse        = "booking_intent_parse"
	bookingMCPDefaultStartupTimeout  = 15 * time.Second
	bookingMCPDefaultRequestTimeout  = 10 * time.Second
	bookingMCPDefaultHeaderReadLimit = 8 * 1024
)

var (
	bookingMCPHTTPClientFactory = func(timeout time.Duration) *http.Client {
		if timeout <= 0 {
			timeout = bookingMCPDefaultRequestTimeout
		}
		return &http.Client{Timeout: timeout}
	}
	bookingMCPStatusFn             = bookingServiceStatus
	bookingMCPStartFn              = startBookingServiceInBackground
	bookingMCPFetchCatalogStatusFn = fetchBookingPublicCatalogStatus
)

type bookingMCPServeConfig struct {
	ThirdPartyID   string
	Token          string
	SecurityKeys   string
	PublicBaseURL  string
	RuntimePath    string
	AutoStart      bool
	StartupTimeout time.Duration
}

type bookingMCPServer struct {
	thirdPartyID string
	token        string
	publicBase   string
	httpClient   *http.Client
}

type bookingMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type bookingMCPResponse struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      json.RawMessage     `json:"id"`
	Result  any                 `json:"result,omitempty"`
	Error   *bookingMCPRPCError `json:"error,omitempty"`
}

type bookingMCPRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type bookingMCPToolCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type bookingMCPToolListResult struct {
	Tools []bookingMCPToolDefinition `json:"tools"`
}

type bookingMCPToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type bookingMCPToolCallResult struct {
	Content           []bookingMCPTextContent `json:"content"`
	StructuredContent map[string]any          `json:"structuredContent,omitempty"`
	IsError           bool                    `json:"isError,omitempty"`
}

type bookingMCPTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type bookingMCPToolOutput struct {
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Data       any    `json:"data,omitempty"`
	HTTPStatus int    `json:"http_status"`
}

type bookingMCPToolCatalogArgs struct {
	ProductID   string `json:"product_id,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	IncludeFull *bool  `json:"include_full,omitempty"`
}

type bookingMCPToolReservationsArgs struct {
	UserID string `json:"user_id,omitempty"`
	Status string `json:"status,omitempty"`
}

type bookingMCPToolIntentParseArgs struct {
	UserID         string `json:"user_id"`
	Content        string `json:"content"`
	Channel        string `json:"channel,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func newBookingMCPCommand(app *AppContext) *cobra.Command {
	var (
		thirdPartyID   string
		token          string
		securityKeys   string
		publicURL      string
		runtimePath    string
		autoStart      bool
		startupTimeout time.Duration
	)

	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve booking MCP tools over local stdio",
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start booking MCP server (stdio)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := bookingMCPServeConfig{
				ThirdPartyID:   strings.TrimSpace(thirdPartyID),
				Token:          strings.TrimSpace(token),
				SecurityKeys:   strings.TrimSpace(securityKeys),
				PublicBaseURL:  strings.TrimSpace(publicURL),
				RuntimePath:    strings.TrimSpace(runtimePath),
				AutoStart:      autoStart,
				StartupTimeout: startupTimeout,
			}
			if app != nil {
				app.SetExecution("booking", []string{"mcp", "serve"})
				app.SetResult(map[string]any{
					"mode":             "booking_mcp_serve",
					"third_party_id":   cfg.ThirdPartyID,
					"url":              cfg.PublicBaseURL,
					"runtime":          cfg.RuntimePath,
					"auto_start":       cfg.AutoStart,
					"startup_timeout":  cfg.StartupTimeout.String(),
					"protocol_version": bookingMCPProtocolVersion,
				})
			}
			return runBookingMCPServe(cmd.Context(), cfg, os.Stdin, os.Stdout)
		},
	}

	serveCmd.Flags().StringVar(&thirdPartyID, "third-party-id", "", "Third-party ID used for signed headers (required)")
	serveCmd.Flags().StringVar(&token, "token", "", "Raw token for request signing (overrides --security-keys)")
	serveCmd.Flags().StringVar(&securityKeys, "security-keys", defaultSecurityKeysPath(), "Path to unified security keys JSON")
	serveCmd.Flags().StringVar(&publicURL, "url", "", "Booking public base URL, e.g. http://127.0.0.1:18081")
	serveCmd.Flags().StringVar(&runtimePath, "runtime", defaultBookingRuntimeStatePath(), "Path to booking runtime state JSON")
	serveCmd.Flags().BoolVar(&autoStart, "auto-start", true, "Auto-start booking service if not running")
	serveCmd.Flags().DurationVar(&startupTimeout, "startup-timeout", bookingMCPDefaultStartupTimeout, "Maximum wait for booking service readiness")

	mcpCmd.AddCommand(serveCmd)
	return mcpCmd
}

func runBookingMCPServe(ctx context.Context, cfg bookingMCPServeConfig, in io.Reader, out io.Writer) error {
	server, err := newBookingMCPServer(cfg)
	if err != nil {
		return err
	}
	return server.serve(ctx, in, out)
}

func newBookingMCPServer(cfg bookingMCPServeConfig) (*bookingMCPServer, error) {
	thirdPartyID := normalizeThirdPartyID(cfg.ThirdPartyID)
	if thirdPartyID == "" {
		return nil, fmt.Errorf("third-party-id is required")
	}
	if cfg.StartupTimeout <= 0 {
		return nil, fmt.Errorf("startup-timeout must be > 0")
	}
	if strings.TrimSpace(cfg.SecurityKeys) == "" {
		return nil, fmt.Errorf("security-keys is required")
	}
	if strings.TrimSpace(cfg.RuntimePath) == "" {
		cfg.RuntimePath = defaultBookingRuntimeStatePath()
	}
	token, err := resolveBookingAgentToken(cfg.Token, cfg.SecurityKeys, thirdPartyID)
	if err != nil {
		return nil, err
	}
	baseURL, err := resolveBookingMCPPublicBaseURL(cfg, thirdPartyID, token)
	if err != nil {
		return nil, err
	}
	return &bookingMCPServer{
		thirdPartyID: thirdPartyID,
		token:        token,
		publicBase:   baseURL,
		httpClient:   bookingMCPHTTPClientFactory(bookingMCPDefaultRequestTimeout),
	}, nil
}

func resolveBookingMCPPublicBaseURL(cfg bookingMCPServeConfig, thirdPartyID string, token string) (string, error) {
	if trimmed := strings.TrimSpace(cfg.PublicBaseURL); trimmed != "" {
		return normalizeBookingAgentBaseURL(trimmed)
	}

	status, err := bookingMCPStatusFn(strings.TrimSpace(cfg.RuntimePath))
	if err != nil {
		return "", err
	}
	if status.Running && status.Runtime != nil {
		publicAddress := bookingRuntimePublicAddress(status.Runtime)
		if strings.TrimSpace(publicAddress) == "" {
			return "", fmt.Errorf("booking runtime is missing public address; restart service with `go run . booking service start`")
		}
		baseURL, err := normalizeBookingAgentBaseURL(buildWebhookManagementURL(publicAddress, "/"))
		if err != nil {
			return "", err
		}
		if err := waitBookingMCPRuntimeReady(publicAddress, thirdPartyID, token, cfg.StartupTimeout); err != nil {
			return "", fmt.Errorf("booking service not ready: %w", err)
		}
		return baseURL, nil
	}

	if !cfg.AutoStart {
		return "", fmt.Errorf("booking service is not running; start it with `go run . booking service start` or rerun with --auto-start=true")
	}

	startCfg := newBookingServiceServeConfig()
	startCfg.RuntimePath = strings.TrimSpace(cfg.RuntimePath)
	startCfg.IdempotencyPath = defaultBookingIdempotencyStatePath(startCfg.RuntimePath)
	startCfg.APIKeysPath = strings.TrimSpace(cfg.SecurityKeys)
	if err := startCfg.validate(); err != nil {
		return "", err
	}

	startResult, err := bookingMCPStartFn(startCfg.RuntimePath, defaultBookingServiceLogPath(), startCfg)
	if err != nil {
		return "", fmt.Errorf("auto-start booking service failed: %w; run `go run . booking service start` to inspect logs", err)
	}
	runtime := startResult.Runtime
	if runtime == nil {
		statusAfterStart, statusErr := bookingMCPStatusFn(startCfg.RuntimePath)
		if statusErr != nil {
			return "", fmt.Errorf("booking service started but runtime status check failed: %w", statusErr)
		}
		runtime = statusAfterStart.Runtime
	}
	if runtime == nil {
		return "", fmt.Errorf("booking service start returned without runtime details; run `go run . booking service status`")
	}

	publicAddress := bookingRuntimePublicAddress(runtime)
	if strings.TrimSpace(publicAddress) == "" {
		return "", fmt.Errorf("booking service started but runtime public address is empty")
	}
	if err := waitBookingMCPRuntimeReady(publicAddress, thirdPartyID, token, cfg.StartupTimeout); err != nil {
		return "", fmt.Errorf("booking service auto-start timeout after %s: %w", cfg.StartupTimeout, err)
	}
	return normalizeBookingAgentBaseURL(buildWebhookManagementURL(publicAddress, "/"))
}

func waitBookingMCPRuntimeReady(publicAddress string, thirdPartyID string, token string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = bookingMCPDefaultStartupTimeout
	}
	record := webhookTokenRecord{
		ThirdPartyID: normalizeThirdPartyID(thirdPartyID),
		Token:        strings.TrimSpace(token),
		Scopes:       []string{securityScopeBooking},
	}
	if record.ThirdPartyID == "" || record.Token == "" {
		return fmt.Errorf("runtime readiness check requires booking token and third-party-id")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := bookingMCPFetchCatalogStatusFn(publicAddress, record, 2*time.Second); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("runtime not ready")
}

func (s *bookingMCPServer) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s == nil {
		return fmt.Errorf("booking MCP server is nil")
	}
	if in == nil {
		return fmt.Errorf("booking MCP input stream is nil")
	}
	if out == nil {
		return fmt.Errorf("booking MCP output stream is nil")
	}
	reader := bufio.NewReader(in)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		frame, err := readMCPFrame(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		if err := s.handleFrame(ctx, frame, out); err != nil {
			return err
		}
	}
}

func (s *bookingMCPServer) handleFrame(ctx context.Context, frame []byte, out io.Writer) error {
	var req bookingMCPRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error: &bookingMCPRPCError{
				Code:    -32700,
				Message: "parse error",
				Data:    strings.TrimSpace(err.Error()),
			},
		})
	}
	id, hasResponse := normalizeMCPID(req.ID)
	if strings.TrimSpace(req.JSONRPC) != "2.0" {
		if !hasResponse {
			return nil
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &bookingMCPRPCError{
				Code:    -32600,
				Message: "invalid request",
				Data:    "jsonrpc must be 2.0",
			},
		})
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		if !hasResponse {
			return nil
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &bookingMCPRPCError{
				Code:    -32600,
				Message: "invalid request",
				Data:    "method is required",
			},
		})
	}

	switch method {
	case "initialize":
		if !hasResponse {
			return nil
		}
		result := map[string]any{
			"protocolVersion": bookingMCPProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    bookingMCPServerName,
				"version": currentBuildVersion(),
			},
			"instructions": "Use booking tools to query catalog/reservations and parse intents. This server does not expose write actions in phase one.",
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result:  result,
		})
	case "notifications/initialized":
		return nil
	case "ping":
		if !hasResponse {
			return nil
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result:  map[string]any{},
		})
	case "tools/list":
		if !hasResponse {
			return nil
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result: bookingMCPToolListResult{
				Tools: bookingMCPToolDefinitions(),
			},
		})
	case "tools/call":
		if !hasResponse {
			return nil
		}
		result, rpcErr := s.handleToolsCall(ctx, req.Params)
		if rpcErr != nil {
			return writeMCPResponse(out, bookingMCPResponse{
				JSONRPC: "2.0",
				ID:      id,
				Error:   rpcErr,
			})
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result:  result,
		})
	default:
		if !hasResponse {
			return nil
		}
		return writeMCPResponse(out, bookingMCPResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &bookingMCPRPCError{
				Code:    -32601,
				Message: "method not found",
				Data:    method,
			},
		})
	}
}

func (s *bookingMCPServer) handleToolsCall(ctx context.Context, rawParams json.RawMessage) (bookingMCPToolCallResult, *bookingMCPRPCError) {
	var callReq bookingMCPToolCallRequest
	if len(bytes.TrimSpace(rawParams)) == 0 {
		return bookingMCPToolCallResult{}, &bookingMCPRPCError{
			Code:    -32602,
			Message: "invalid params",
			Data:    "tools/call params are required",
		}
	}
	if err := json.Unmarshal(rawParams, &callReq); err != nil {
		return bookingMCPToolCallResult{}, &bookingMCPRPCError{
			Code:    -32602,
			Message: "invalid params",
			Data:    strings.TrimSpace(err.Error()),
		}
	}
	toolName := strings.TrimSpace(callReq.Name)
	if toolName == "" {
		return bookingMCPToolCallResult{}, &bookingMCPRPCError{
			Code:    -32602,
			Message: "invalid params",
			Data:    "tool name is required",
		}
	}

	var output bookingMCPToolOutput
	var err error
	switch toolName {
	case bookingMCPToolCatalogQuery:
		output, err = s.callToolCatalogQuery(ctx, callReq.Arguments)
	case bookingMCPToolReservationsList:
		output, err = s.callToolReservationsList(ctx, callReq.Arguments)
	case bookingMCPToolIntentParse:
		output, err = s.callToolIntentParse(ctx, callReq.Arguments)
	default:
		return bookingMCPToolCallResult{}, &bookingMCPRPCError{
			Code:    -32602,
			Message: "unknown tool",
			Data:    toolName,
		}
	}

	if err != nil {
		output = bookingMCPToolOutput{
			OK:         false,
			Code:       publicAPIErrorCodeInternalError,
			Message:    strings.TrimSpace(err.Error()),
			HTTPStatus: http.StatusBadGateway,
		}
	}

	encoded, marshalErr := marshalJSONNoHTMLEscape(output)
	if marshalErr != nil {
		encoded = []byte(`{"ok":false,"code":"internal_error","message":"marshal result failed","http_status":500}`)
	}
	result := bookingMCPToolCallResult{
		Content: []bookingMCPTextContent{
			{
				Type: "text",
				Text: string(encoded),
			},
		},
		StructuredContent: map[string]any{
			"ok":          output.OK,
			"code":        output.Code,
			"message":     output.Message,
			"request_id":  output.RequestID,
			"data":        output.Data,
			"http_status": output.HTTPStatus,
		},
	}
	if !output.OK {
		result.IsError = true
	}
	return result, nil
}

func (s *bookingMCPServer) callToolCatalogQuery(ctx context.Context, rawArgs json.RawMessage) (bookingMCPToolOutput, error) {
	args := bookingMCPToolCatalogArgs{}
	if err := decodeMCPToolArgs(rawArgs, &args); err != nil {
		return bookingMCPToolOutput{}, err
	}

	values := url.Values{}
	if trimmed := strings.TrimSpace(args.ProductID); trimmed != "" {
		values.Set("product_id", trimmed)
	}
	if trimmed := strings.TrimSpace(args.From); trimmed != "" {
		values.Set("from", trimmed)
	}
	if trimmed := strings.TrimSpace(args.To); trimmed != "" {
		values.Set("to", trimmed)
	}
	if args.IncludeFull != nil {
		values.Set("include_full", strconv.FormatBool(*args.IncludeFull))
	}
	targetURL, err := buildBookingMCPToolURL(s.publicBase, bookingPublicCatalogPath, values)
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	envelope, statusCode, err := s.signedPublicRequest(ctx, http.MethodGet, targetURL, nil, "")
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	return bookingMCPOutputFromEnvelope(statusCode, envelope), nil
}

func (s *bookingMCPServer) callToolReservationsList(ctx context.Context, rawArgs json.RawMessage) (bookingMCPToolOutput, error) {
	args := bookingMCPToolReservationsArgs{}
	if err := decodeMCPToolArgs(rawArgs, &args); err != nil {
		return bookingMCPToolOutput{}, err
	}

	values := url.Values{}
	if trimmed := strings.TrimSpace(args.UserID); trimmed != "" {
		values.Set("user_id", trimmed)
	}
	if trimmed := strings.TrimSpace(args.Status); trimmed != "" {
		values.Set("status", trimmed)
	}
	targetURL, err := buildBookingMCPToolURL(s.publicBase, bookingPublicReservationsPath, values)
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	envelope, statusCode, err := s.signedPublicRequest(ctx, http.MethodGet, targetURL, nil, "")
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	return bookingMCPOutputFromEnvelope(statusCode, envelope), nil
}

func (s *bookingMCPServer) callToolIntentParse(ctx context.Context, rawArgs json.RawMessage) (bookingMCPToolOutput, error) {
	args := bookingMCPToolIntentParseArgs{}
	if err := decodeMCPToolArgs(rawArgs, &args); err != nil {
		return bookingMCPToolOutput{}, err
	}
	args.UserID = strings.TrimSpace(args.UserID)
	args.Content = strings.TrimSpace(args.Content)
	args.Channel = strings.TrimSpace(args.Channel)
	args.IdempotencyKey = strings.TrimSpace(args.IdempotencyKey)
	if args.UserID == "" {
		return bookingMCPToolOutput{
			OK:         false,
			Code:       publicAPIErrorCodeValidationError,
			Message:    "user_id is required",
			HTTPStatus: http.StatusBadRequest,
		}, nil
	}
	if args.Content == "" {
		return bookingMCPToolOutput{
			OK:         false,
			Code:       publicAPIErrorCodeValidationError,
			Message:    "content is required",
			HTTPStatus: http.StatusBadRequest,
		}, nil
	}
	if args.Channel == "" {
		args.Channel = "chat"
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = fmt.Sprintf("booking-mcp-parse-%d", time.Now().UnixNano())
	}
	payload := bookingIntentParseRequest{
		UserID:  args.UserID,
		Content: args.Content,
		Channel: args.Channel,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	targetURL, err := buildBookingMCPToolURL(s.publicBase, bookingPublicIntentParsePath, nil)
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	envelope, statusCode, err := s.signedPublicRequest(ctx, http.MethodPost, targetURL, body, args.IdempotencyKey)
	if err != nil {
		return bookingMCPToolOutput{}, err
	}
	result := bookingMCPOutputFromEnvelope(statusCode, envelope)
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	if dataMap, ok := result.Data.(map[string]any); ok {
		dataMap[publicAPIHeaderIdempotencyKey] = args.IdempotencyKey
		result.Data = dataMap
	} else {
		result.Data = map[string]any{
			"value":                       result.Data,
			publicAPIHeaderIdempotencyKey: args.IdempotencyKey,
		}
	}
	return result, nil
}

func (s *bookingMCPServer) signedPublicRequest(
	ctx context.Context,
	method string,
	targetURL string,
	body []byte,
	idempotencyKey string,
) (publicAPIEnvelope, int, error) {
	if s == nil {
		return publicAPIEnvelope{}, 0, fmt.Errorf("booking MCP server is nil")
	}
	if s.httpClient == nil {
		s.httpClient = bookingMCPHTTPClientFactory(bookingMCPDefaultRequestTimeout)
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return publicAPIEnvelope{}, 0, fmt.Errorf("http method is required")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := computeWebhookSignature(s.thirdPartyID, timestamp, s.token, body)

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return publicAPIEnvelope{}, 0, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(webhookHeaderThirdPartyID, s.thirdPartyID)
	req.Header.Set(webhookHeaderTimestamp, timestamp)
	req.Header.Set(webhookHeaderSignature, signature)
	if method == http.MethodPost {
		req.Header.Set(publicAPIHeaderIdempotencyKey, strings.TrimSpace(idempotencyKey))
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return publicAPIEnvelope{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return publicAPIEnvelope{}, resp.StatusCode, err
	}
	envelope, err := readPublicAPIEnvelope(raw)
	if err != nil {
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) > 512 {
			trimmed = trimmed[:512]
		}
		return publicAPIEnvelope{}, resp.StatusCode, fmt.Errorf("response is not valid public envelope: http=%d body=%s", resp.StatusCode, trimmed)
	}
	return envelope, resp.StatusCode, nil
}

func decodeMCPToolArgs(raw json.RawMessage, target any) error {
	if target == nil {
		return fmt.Errorf("tool args target is required")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func buildBookingMCPToolURL(baseURL string, endpointPath string, query url.Values) (string, error) {
	targetURL, err := buildBookingAgentEndpointURL(baseURL, endpointPath)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func bookingMCPOutputFromEnvelope(httpStatus int, envelope publicAPIEnvelope) bookingMCPToolOutput {
	ok := strings.TrimSpace(envelope.Status) == publicAPIStatusOK
	code := strings.TrimSpace(envelope.Code)
	if code == "" {
		if ok {
			code = publicAPIErrorCodeOK
		} else {
			code = publicAPIErrorCodeInternalError
		}
	}
	return bookingMCPToolOutput{
		OK:         ok,
		Code:       code,
		Message:    strings.TrimSpace(envelope.Message),
		RequestID:  strings.TrimSpace(envelope.RequestID),
		Data:       envelope.Data,
		HTTPStatus: httpStatus,
	}
}

func bookingMCPToolDefinitions() []bookingMCPToolDefinition {
	return []bookingMCPToolDefinition{
		{
			Name:        bookingMCPToolCatalogQuery,
			Description: "Query booking catalog via GET /booking/catalog",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"product_id": map[string]any{
						"type":        "string",
						"description": "Optional product ID filter",
					},
					"from": map[string]any{
						"type":        "string",
						"description": "Optional RFC3339 start boundary",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "Optional RFC3339 end boundary",
					},
					"include_full": map[string]any{
						"type":        "boolean",
						"description": "Include slots with zero availability",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        bookingMCPToolReservationsList,
			Description: "List reservations via GET /booking/reservations",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user_id": map[string]any{
						"type":        "string",
						"description": "Optional reservation owner filter",
					},
					"status": map[string]any{
						"type":        "string",
						"description": "Optional reservation status filter",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        bookingMCPToolIntentParse,
			Description: "Parse booking intent via POST /booking/intents/parse",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user_id": map[string]any{
						"type":        "string",
						"description": "Booking user ID",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Natural language booking content",
					},
					"channel": map[string]any{
						"type":        "string",
						"description": "Channel name, defaults to chat",
					},
					"idempotency_key": map[string]any{
						"type":        "string",
						"description": "Optional Idempotency-Key for parse",
					},
				},
				"required":             []string{"user_id", "content"},
				"additionalProperties": false,
			},
		},
	}
}

func normalizeMCPID(raw json.RawMessage) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	return raw, true
}

func readMCPFrame(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, io.EOF
	}
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(line) == 0 {
				return nil, io.EOF
			}
			return nil, err
		}
		if len(line) > bookingMCPDefaultHeaderReadLimit {
			return nil, fmt.Errorf("MCP header line too long")
		}
		trimmedLine := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmedLine) == "" {
			break
		}
		parts := strings.SplitN(trimmedLine, ":", 2)
		if len(parts) != 2 {
			continue
		}
		headerKey := strings.ToLower(strings.TrimSpace(parts[0]))
		headerValue := strings.TrimSpace(parts[1])
		if headerKey != "content-length" {
			continue
		}
		length, err := strconv.Atoi(headerValue)
		if err != nil || length < 0 {
			return nil, fmt.Errorf("invalid Content-Length %q", headerValue)
		}
		contentLength = length
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeMCPResponse(out io.Writer, response bookingMCPResponse) error {
	if out == nil {
		return fmt.Errorf("MCP output stream is nil")
	}
	if strings.TrimSpace(response.JSONRPC) == "" {
		response.JSONRPC = "2.0"
	}
	encoded, err := marshalJSONNoHTMLEscape(response)
	if err != nil {
		return err
	}
	frameHeader := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(encoded))
	if _, err := io.WriteString(out, frameHeader); err != nil {
		return err
	}
	_, err = out.Write(encoded)
	return err
}
