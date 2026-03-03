// Package plugin - Core Datasource Implementation
//
// This package contains the core implementation of the VictoriaLogs Grafana datasource.
// It handles all communication between Grafana and VictoriaLogs, including query execution,
// health checks, streaming (live tail), and resource API calls.
//
// ARCHITECTURE OVERVIEW:
// The plugin uses a two-tier structure:
//  1. Datasource - A singleton that manages DatasourceInstance objects via InstanceManager
//  2. DatasourceInstance - Per-datasource configuration created once and cached
//
// WHY InstanceManager?
// Grafana supports multiple instances of the same datasource type (e.g., multiple VL endpoints).
// The InstanceManager caches instances to avoid recreating HTTP clients and parsing settings
// on every request. Each instance has its own:
//   - HTTP client (with connection pooling)
//   - Streaming HTTP client (for live tail, with different timeout settings)
//   - Parsed settings (URL, headers, multitenancy config)
//
// REQUEST FLOW:
//  1. Grafana sends request → Datasource method (QueryData, CheckHealth, etc.)
//  2. Datasource.getInstance() retrieves or creates cached DatasourceInstance
//  3. DatasourceInstance executes the actual request to VictoriaLogs
//  4. Response is parsed and returned as Grafana data frames
//
// RESOURCE API (CallResourceHandler):
// The plugin exposes HTTP endpoints for frontend-to-backend communication:
//   - /select/logsql/field_values - Get unique values for a field
//   - /select/logsql/field_names - Get list of available field names
//   - /select/logsql/streams - Get list of log streams
//   - /select/logsql/stream_field_names - Get stream field names
//   - /select/logsql/stream_field_values - Get stream field values
//   - /select/tenant_ids - Get list of tenant IDs (for multitenancy)
//   - /vmui - Get VMUI URL for native VictoriaLogs UI
//
// For more details, see:
//   - onboarding/onboarding-backend.md
//   - onboarding/onboarding-query-flow.md

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// Compile-time interface assertions
// These ensure that our Datasource and DatasourceInstance types implement
// all required Grafana plugin interfaces. If any method is missing or has
// the wrong signature, compilation will fail with a clear error message.
var (
	_ backend.StreamHandler         = &Datasource{}
	_ backend.QueryDataHandler      = &Datasource{}
	_ backend.CheckHealthHandler    = &Datasource{}
	_ instancemgmt.InstanceDisposer = &DatasourceInstance{}
)

// Constants for HTTP headers and paths
// These are used throughout the plugin for consistent string references
const (
	health          = "/health"
	httpHeaderName  = "httpHeaderName"
	httpHeaderValue = "httpHeaderValue"
	// requestFromAlert is a header key that Grafana uses to indicate a query is from alerting.
	// This is a Grafana-specific convention - alerts may need different handling than regular queries.
	// For example, we return different frame formats for alert queries (numeric multi vs time series).
	requestFromAlert = "FromAlert"
	// accountIDHeader and projectIDHeader are used for VictoriaMetrics multitenancy.
	// VictoriaLogs clusters can be multitenant, where each tenant has an AccountID and ProjectID.
	// These are passed as HTTP headers and used for data isolation between tenants.
	accountIDHeader = "AccountID"
	projectIDHeader = "ProjectID"
)

// Datasource is the main plugin struct that manages all datasource instances.
// It implements four Grafana handler interfaces:
//   - StreamHandler: For live log tailing via WebSocket/gRPC streaming
//   - QueryDataHandler: For executing LogsQL queries
//   - CheckHealthHandler: For the "Test connection" button in datasource config
//   - CallResourceHandler: For HTTP resource API (proxies to VictoriaLogs)
//
// This struct is created once at plugin startup (in main.go) and lives for the
// entire plugin lifetime. It uses an InstanceManager to cache per-datasource
// instances, avoiding repeated HTTP client creation.
type Datasource struct {
	// im is the InstanceManager that creates and caches DatasourceInstance objects.
	// Each Grafana datasource (identified by UID) gets its own instance.
	im instancemgmt.InstanceManager

	// logger provides structured logging for the datasource.
	logger log.Logger

	// CallResourceHandler is embedded to handle HTTP resource requests from the frontend.
	// It's implemented using httpadapter which adapts a standard http.ServeMux to
	// Grafana's CallResourceHandler interface.
	backend.CallResourceHandler
}

// NewDatasource creates a new Datasource instance and sets up HTTP routing.
// This is called once from main.go when the plugin starts.
//
// The function:
//  1. Creates an InstanceManager to handle per-datasource instance lifecycle
//  2. Sets up an HTTP mux (router) for resource API calls
//  3. Registers handlers for various VictoriaLogs API endpoints
//
// The HTTP routes defined here are used by the frontend to query metadata:
//   - /select/logsql/field_values - Query unique values for a field (used in dropdowns)
//   - /select/logsql/field_names - List all available field names (for autocomplete)
//   - /select/logsql/streams - List all log streams (for stream selection)
//   - /select/logsql/stream_field_names - List stream-specific fields
//   - /select/logsql/stream_field_values - Get values for stream fields
//   - /select/tenant_ids - List tenant IDs for multitenant setups
//   - /vmui - Return URL for VictoriaLogs native UI
func NewDatasource() *Datasource {
	var ds Datasource
	// Create the instance manager with our factory function.
	// The factory will be called once per unique datasource UID.
	ds.im = datasource.NewInstanceManager(newDatasourceInstance)
	ds.logger = log.New()

	// Set up HTTP routing for the resource API.
	// These endpoints are called by the frontend via backend.datasource resource calls.
	mux := http.NewServeMux()
	mux.HandleFunc("/", ds.RootHandler)
	mux.HandleFunc("/select/logsql/field_values", ds.VLAPIQuery)
	mux.HandleFunc("/select/logsql/field_names", ds.VLAPIQuery)
	mux.HandleFunc("/select/logsql/streams", ds.VLAPIQuery)
	mux.HandleFunc("/select/logsql/stream_field_names", ds.VLAPIQuery)
	mux.HandleFunc("/select/logsql/stream_field_values", ds.VLAPIQuery)
	mux.HandleFunc("/select/tenant_ids", ds.VLAPITenantIDs)
	mux.HandleFunc("/vmui", ds.VMUIQuery)

	// Wrap the mux with httpadapter to make it compatible with Grafana's CallResourceHandler.
	// This converts standard HTTP requests/responses to Grafana's backend protocol.
	ds.CallResourceHandler = httpadapter.New(mux)
	return &ds
}

// newDatasourceInstance is the factory function for creating DatasourceInstance objects.
// It's called by the InstanceManager when a new datasource UID is encountered.
// The instance is then cached and reused for all subsequent requests to that datasource.
//
// This function performs expensive one-time setup:
//   - Parses datasource settings (URL, custom headers, multitenancy config)
//   - Creates HTTP clients with proper timeouts and connection pooling
//   - Creates a separate streaming client for live tail (with no timeout)
//
// Parameters:
//   - ctx: Request context (for cancellation propagation)
//   - settings: Grafana's datasource configuration (URL, JSON settings, secure credentials)
//
// Returns an instancemgmt.Instance (interface) which is our *DatasourceInstance.
func newDatasourceInstance(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	logger := log.New()
	logger.Debug("Initializing new data source instance")

	// Build HTTP client options from Grafana settings.
	// This includes custom headers, TLS config, proxy settings, etc.
	opts, err := settings.HTTPClientOptions(ctx)
	if err != nil {
		logger.Error("Error parsing VL settings", "error", err)
		return nil, err
	}

	// Enable forwarding of HTTP headers from Grafana to VictoriaLogs.
	// This allows headers like Authorization to be passed through.
	opts.ForwardHTTPHeaders = true

	// Clean up any empty header keys that might have been accidentally configured.
	for key := range opts.Header {
		if key == "" {
			opts.Header.Del(key)
		}
	}

	// Create the main HTTP client for regular queries.
	// This client has standard timeouts configured by Grafana.
	cl, err := httpclient.New(opts)
	if err != nil {
		logger.Error("error initializing HTTP client", "error", err)
		return nil, err
	}

	// Create a separate HTTP client for streaming (live tail).
	// We disable timeout (Timeout = 0) because streaming connections are long-lived.
	// The connection stays open and receives log lines as they arrive.
	opts.Timeouts.Timeout = 0
	strCl, err := httpclient.New(opts)
	if err != nil {
		logger.Error("error initializing HTTP client", "error", err)
		return nil, err
	}

	// Parse the datasource-specific settings from JSONData.
	var dstSettings DataSourceInstanceSettings
	if dstSettings, err = buildDatasourceSettings(settings); err != nil {
		return nil, fmt.Errorf("failed to copy datasource settings: %w", err)
	}

	// Derive the VMUI URL from the datasource URL if not explicitly set.
	if err := setVmuiURL(&dstSettings); err != nil {
		return nil, fmt.Errorf("failed to set vmui url: %w", err)
	}

	// Parse Grafana-specific settings including custom headers and multitenancy.
	grafanaSettings, err := NewGrafanaSettings(settings)
	if err != nil {
		logger.Error("error create a new GrafanaSettings", "error", err)
		return nil, err
	}

	return &DatasourceInstance{
		settings:            dstSettings,
		httpClient:          cl,
		httpStreamingClient: strCl,
		grafanaSettings:     grafanaSettings,
	}, nil
}

// MultitenancyHeaders holds the tenant identification headers for multitenant VictoriaLogs.
// In a multitenant deployment, each tenant's data is isolated using these identifiers.
// The headers are sent with every request to VictoriaLogs to ensure proper data access.
type MultitenancyHeaders struct {
	// AccountID is the primary tenant identifier (also known as TenantID in VM docs).
	// Defaults to "0" if not specified.
	AccountID string `json:"AccountID"`
	// ProjectID is a secondary identifier for further subdivision within a tenant.
	// This allows organizing data within a single tenant into projects.
	// Defaults to "0" if not specified.
	ProjectID string `json:"ProjectID"`
}

// GrafanaSettings contains all configuration parsed from Grafana's datasource settings.
// This includes both the JSON configuration and secure (encrypted) values.
// These settings are parsed once at instance creation and reused for all requests.
type GrafanaSettings struct {
	// HTTPMethod is the HTTP method to use for queries (GET or POST).
	// GET is the default and works well for most queries.
	// POST may be needed for very long queries that exceed URL length limits.
	HTTPMethod string `json:"httpMethod"`

	// QueryParams contains additional URL query parameters to append to every request.
	// Users can configure these in the datasource settings under "Custom query parameters".
	QueryParams string `json:"customQueryParameters"`

	// CustomHeaders contains all custom headers to send with requests.
	// This includes both user-configured headers and multitenancy headers.
	// Headers from DecryptedSecureJSONData are merged in for sensitive values.
	CustomHeaders http.Header `json:"-"`

	// MultitenancyHeaders contains the tenant identification for multitenant deployments.
	MultitenancyHeaders MultitenancyHeaders `json:"-"`
}

// NewGrafanaSettings parses all Grafana datasource settings into a structured format.
// It handles:
//   - Custom HTTP headers (combining public JSONData and encrypted SecureJSONData)
//   - Multitenancy headers (AccountID, ProjectID)
//   - Default values for optional settings
//
// The secure JSON data contains sensitive values like API keys that shouldn't be
// visible in the UI. Grafana encrypts these, and we only have access to decrypted values.
func NewGrafanaSettings(settings backend.DataSourceInstanceSettings) (*GrafanaSettings, error) {
	// Parse custom headers from both JSONData (header names) and SecureJSONData (header values).
	// This split keeps sensitive values encrypted in Grafana's database.
	customHttpHeaders, err := parseCustomHeaders(settings.JSONData, settings.DecryptedSecureJSONData)
	if err != nil {
		return nil, fmt.Errorf("error parse custom headers: %w", err)
	}

	var grafanaSettings GrafanaSettings
	if err := json.Unmarshal(settings.JSONData, &grafanaSettings); err != nil {
		return nil, fmt.Errorf("failed to parse datasource settings: %w", err)
	}

	// Parse multitenancy headers (AccountID and ProjectID).
	multitenancyHeaders, err := parseMultitenancyHeaders(settings)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tenant settings: %w", err)
	}
	grafanaSettings.MultitenancyHeaders = multitenancyHeaders

	// Merge multitenancy headers into the common CustomHeaders set.
	// This ensures they're included in every request without special handling.
	customHttpHeaders.Set(projectIDHeader, grafanaSettings.MultitenancyHeaders.ProjectID)
	customHttpHeaders.Set(accountIDHeader, grafanaSettings.MultitenancyHeaders.AccountID)

	grafanaSettings.CustomHeaders = customHttpHeaders

	// Default to GET method if not specified.
	// GET is preferred because it's cacheable and simpler.
	if grafanaSettings.HTTPMethod == "" {
		grafanaSettings.HTTPMethod = http.MethodGet
	}
	return &grafanaSettings, nil
}

// DatasourceInstance is a per-datasource configuration that handles actual requests.
// One instance is created per unique datasource UID and cached by the InstanceManager.
//
// The instance holds:
//   - HTTP clients (one for regular queries, one for streaming)
//   - Parsed settings (URL, VMUI URL)
//   - Grafana-specific settings (headers, multitenancy)
//   - Live mode state (channels for streaming responses)
//
// IMPORTANT: This struct is shared across concurrent requests. The sync.Map for
// liveModeResponses is safe for concurrent access, but other fields should be
// treated as read-only after initialization.
type DatasourceInstance struct {
	// settings contains the VictoriaLogs-specific configuration.
	settings DataSourceInstanceSettings

	// httpClient is used for regular (non-streaming) queries.
	// It has standard timeouts configured by Grafana.
	httpClient *http.Client

	// httpStreamingClient is used for live tail streaming.
	// It has no timeout to support long-lived connections.
	httpStreamingClient *http.Client

	// grafanaSettings contains all parsed configuration from Grafana.
	grafanaSettings *GrafanaSettings

	// liveModeResponses stores channels for streaming query results.
	// Key: request path (requestId/refId format)
	// Value: chan *data.Frame for sending log lines to subscribers.
	// This is a sync.Map because it's accessed concurrently from multiple goroutines.
	liveModeResponses sync.Map
}

// DataSourceInstanceSettings contains VictoriaLogs-specific configuration.
// These are stored in Grafana's database and passed to the plugin on startup.
type DataSourceInstanceSettings struct {
	// URL is the base URL of the VictoriaLogs server.
	// Example: "http://victorialogs:9428" or "http://victorialogs:9428/select/0/logsql"
	// The URL may already include the /select/<accountID>/<projectID> path prefix.
	URL string `json:"URL,omitempty"`

	// VMUIURL is the URL for accessing VictoriaLogs' built-in UI (VMUI).
	// This is used to provide a "Open in VMUI" link in Grafana.
	// If not explicitly set, it's derived from URL by appending "/select/vmui/".
	VMUIURL string `json:"vmuiUrl,omitempty"`
}

// SubscribeStream is called when a client (Grafana frontend) subscribes to a streaming channel.
// This is the first step in setting up a live tail connection.
//
// The flow for live tail:
//  1. Frontend calls runLiveQueryThroughBackend() which triggers SubscribeStream
//  2. We create a channel and store it in liveModeResponses map
//  3. Grafana then calls RunStream (which blocks and streams data)
//  4. Log lines are sent through the channel to the frontend
//
// The channel is keyed by req.Path which is formatted as "${requestId}/${query.refId}"
// in the frontend. This allows multiple concurrent live tail queries.
func (d *Datasource) SubscribeStream(ctx context.Context, req *backend.SubscribeStreamRequest) (*backend.SubscribeStreamResponse, error) {
	di, err := d.getInstance(ctx, req.PluginContext)
	if err != nil {
		return nil, err
	}
	// Create a buffered channel (size 1) to hold one frame at a time.
	// Buffering prevents blocking if the consumer is momentarily slow.
	ch := make(chan *data.Frame, 1)
	di.liveModeResponses.Store(req.Path, ch)
	return &backend.SubscribeStreamResponse{
		Status: backend.SubscribeStreamStatusOK,
	}, nil
}

// getInstance retrieves the cached DatasourceInstance for this datasource.
// If the instance doesn't exist yet, the InstanceManager will create it using
// the newDatasourceInstance factory function.
//
// This method is called by every public handler method to get access to
// the HTTP client and settings for the specific datasource.
func (d *Datasource) getInstance(ctx context.Context, pluginContext backend.PluginContext) (*DatasourceInstance, error) {
	instance, err := d.im.Get(ctx, pluginContext)
	if err != nil {
		return nil, err
	}
	return instance.(*DatasourceInstance), nil
}

// PublishStream is called when a client tries to publish data to a channel.
// We always deny publish requests because this is a read-only datasource.
// Clients can only subscribe to receive data, not send data.
func (d *Datasource) PublishStream(_ context.Context, _ *backend.PublishStreamRequest) (*backend.PublishStreamResponse, error) {
	return &backend.PublishStreamResponse{
		Status: backend.PublishStreamStatusPermissionDenied,
	}, nil
}

// RunStream is the core streaming method that handles live tail connections.
// It's called once when the first client subscribes and runs until all clients
// disconnect or an error occurs.
//
// STREAMING ARCHITECTURE:
// The method sets up two concurrent processes:
//  1. A goroutine that reads frames from the livestream channel and sends them to Grafana
//  2. The main goroutine that reads the VictoriaLogs tail endpoint and parses responses
//
// WHY TWO GOROUTINES?
// The tail endpoint returns a continuous stream of NDJSON (newline-delimited JSON).
// We parse each line into a frame and send it through a channel. The sender goroutine
// then forwards frames to Grafana's streaming infrastructure. This separation allows
// for efficient frame caching (using SameSchema optimization) and proper cleanup.
//
// ERROR HANDLING:
// The method checks for context cancellation errors, which are normal when clients
// disconnect. Other errors are logged and cause the stream to terminate.
func (d *Datasource) RunStream(ctx context.Context, req *backend.RunStreamRequest, sender *backend.StreamSender) error {
	di, err := d.getInstance(ctx, req.PluginContext)
	if err != nil {
		return err
	}
	// Retrieve the channel created in SubscribeStream.
	// The path format is "${requestId}/${query.refId}" - see frontend's runLiveQueryThroughBackend.
	ch, ok := di.liveModeResponses.Load(req.Path)
	if !ok {
		return fmt.Errorf("failed to find the channel for the query: %s", req.Path)
	}
	livestream := ch.(chan *data.Frame)

	// Start a goroutine to send frames to Grafana.
	// This runs until the channel is closed (when streamQuery finishes).
	go func() {
		// prev caches the previous frame's schema for optimization.
		// If consecutive frames have the same schema, we can send just the data bytes
		// instead of the full frame structure, reducing network traffic.
		prev := data.FrameJSONCache{}
		var err error
		for frame := range livestream {
			next, _ := data.FrameToJSONCache(frame)
			if next.SameSchema(&prev) {
				// Same schema as previous - send only bytes (more efficient)
				err = sender.SendBytes(next.Bytes(data.IncludeAll))
			} else {
				// Different schema - send full frame
				err = sender.SendFrame(frame, data.IncludeAll)
			}
			prev = next

			if err != nil {
				// Check if this is a context cancellation (normal client disconnect).
				// This error string is from gRPC - there's no exported error type to compare.
				if strings.Contains(err.Error(), "rpc error: code = Canceled desc = context canceled") {
					backend.Logger.Debug("Client has canceled the request")
					break
				}
				backend.Logger.Error("Failed send frame", "error", err)
			}
		}
	}()

	// streamQuery blocks until the connection is closed or an error occurs.
	// It reads from VictoriaLogs' /select/logsql/tail endpoint.
	if err := di.streamQuery(ctx, req); err != nil {
		return fmt.Errorf("failed to parse stream response: %w", err)
	}

	return nil
}

// Dispose is called when the datasource instance is being destroyed.
// This happens when:
//   - The datasource configuration is changed (URL, headers, etc.)
//   - The plugin is being shut down
//   - Grafana is being shut down
//
// Cleanup tasks:
//   - Close idle HTTP connections to free resources
//   - Close all live tail channels (signals goroutines to stop)
//   - Clear the liveModeResponses map
func (di *DatasourceInstance) Dispose() {
	// Close idle connections in both HTTP clients.
	// This is important for proper resource cleanup and avoiding connection leaks.
	di.httpClient.CloseIdleConnections()
	di.httpStreamingClient.CloseIdleConnections()

	// Close all channels before clearing the map.
	// Closing a channel signals to any goroutines reading from it that the stream has ended.
	di.liveModeResponses.Range(func(key, value interface{}) bool {
		ch := value.(chan *data.Frame)
		close(ch)
		return true
	})
	// Clear the map to free memory and prevent any future access.
	di.liveModeResponses.Clear()
}

// QueryData handles multiple queries in a single request.
// Grafana panels can have multiple queries, and this method executes them concurrently.
//
// CONCURRENT EXECUTION:
// Each query is executed in its own goroutine for parallelism. This is important
// because VictoriaLogs queries can be slow, and we don't want one slow query to
// block others.
//
// ALERTING SUPPORT:
// The method checks if the request is from Grafana's alerting system via headers.
// Alert queries may need different handling (e.g., different frame formats).
//
// Parameters:
//   - req.Queries: List of queries to execute, each with a unique RefID
//
// Returns:
//   - response.Responses: Map of RefID to DataResponse (each containing Frames)
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	headers := req.Headers

	di, err := d.getInstance(ctx, req.PluginContext)
	if err != nil {
		return nil, err
	}

	// Check if this request is from Grafana's alerting system.
	// Alert queries have slightly different requirements.
	forAlerting, err := checkAlertingRequest(headers)
	if err != nil {
		return nil, err
	}

	// Execute queries concurrently using goroutines.
	var wg sync.WaitGroup
	var mu sync.Mutex // Protects response.Responses from concurrent writes

	for _, q := range req.Queries {
		// Parse the raw JSON query into our Query struct.
		rawQuery, err := getQueryFromRaw(q.JSON, forAlerting)
		if err != nil {
			return nil, err
		}
		rawQuery.DataQuery = q // Copy the base DataQuery fields

		wg.Add(1)
		go func(rawQuery *Query) {
			defer wg.Done()
			// Execute the query and get the response.
			resp := di.query(ctx, rawQuery)
			// Thread-safe write to the responses map.
			mu.Lock()
			response.Responses[rawQuery.RefID] = resp
			mu.Unlock()
		}(rawQuery)
	}
	// Wait for all queries to complete before returning.
	wg.Wait()

	return response, nil
}

// streamQuery executes a streaming query to VictoriaLogs and parses the results.
// It connects to the /select/logsql/tail endpoint which returns a continuous stream
// of log lines as NDJSON (newline-delimited JSON).
//
// The method blocks until:
//   - The context is cancelled (client disconnects)
//   - An error occurs
//   - The VictoriaLogs connection closes
//
// Each parsed log line is sent as a data.Frame through the livestream channel,
// which is then forwarded to Grafana's streaming infrastructure.
func (di *DatasourceInstance) streamQuery(ctx context.Context, request *backend.RunStreamRequest) error {
	q, err := getQueryFromRaw(request.Data, false)
	if err != nil {
		return err
	}

	// Execute the streaming query to VictoriaLogs.
	r, err := di.datasourceQuery(ctx, q, true)
	if err != nil {
		return err
	}

	if r == nil {
		// VictoriaLogs returned no data (empty response with Content-Length: 0)
		return nil
	}

	// Ensure the response body is closed when we're done.
	defer func() {
		if err := r.Close(); err != nil {
			backend.Logger.Error("failed to close response body", "err", err.Error())
		}
	}()

	// Get the channel for this query path.
	ch, ok := di.liveModeResponses.Load(request.Path)
	if !ok {
		return fmt.Errorf("failed to find the channel for the query: %s", request.Path)
	}

	livestream := ch.(chan *data.Frame)
	// Parse the NDJSON stream and send frames to the channel.
	return parseStreamResponse(r, livestream)
}

// getQueryFromRaw parses the raw JSON query data into a Query struct.
// This is used for both regular queries and streaming queries.
//
// Parameters:
//   - data: Raw JSON bytes containing the query definition
//   - forAlerting: Whether this query is from Grafana's alerting system
//
// The forAlerting flag affects how responses are formatted later.
func getQueryFromRaw(data json.RawMessage, forAlerting bool) (*Query, error) {
	var q Query
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("failed to parse query json: %s", err)
	}
	q.ForAlerting = forAlerting
	return &q, nil
}

// datasourceQuery executes an HTTP request to VictoriaLogs and returns the response body.
// This is a low-level method that handles:
//   - URL construction based on query type
//   - HTTP request creation with proper headers
//   - Retry logic for transient network errors
//   - Error response parsing
//
// Parameters:
//   - isStream: If true, uses the streaming client and tail endpoint
//
// Returns:
//   - io.ReadCloser: The response body (caller must close)
//   - nil response: If VictoriaLogs returned empty content (Content-Length: 0)
func (di *DatasourceInstance) datasourceQuery(ctx context.Context, q *Query, isStream bool) (io.ReadCloser, error) {
	// Build the URL for this query type.
	reqURL, err := q.getQueryURL(di.settings.URL, di.grafanaSettings.QueryParams)
	if err != nil {
		return nil, fmt.Errorf("failed to create request URL: %w", err)
	}

	var client *http.Client
	if isStream {
		// Use the streaming client (no timeout) and switch to the tail endpoint.
		client = di.httpStreamingClient
		reqURL, err = q.queryTailURL(di.settings.URL, di.grafanaSettings.QueryParams)
		if err != nil {
			return nil, fmt.Errorf("failed to create request URL: %w", err)
		}
	} else {
		// Use the regular client (with standard timeouts).
		client = di.httpClient
	}

	// Create the HTTP request with context for cancellation support.
	req, err := http.NewRequestWithContext(ctx, di.grafanaSettings.HTTPMethod, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create new request with context: %w", err)
	}
	// Clone custom headers for this request.
	req.Header = di.grafanaSettings.CustomHeaders.Clone()

	// Execute the request.
	resp, err := client.Do(req)
	if err != nil {
		// Check if this is a transient error that might succeed on retry.
		if !isTrivialError(err) {
			return nil, err
		}

		// Retry once for transient errors (broken pipe, connection reset).
		// These can happen if an intermediate proxy closes idle connections.
		req, err = http.NewRequestWithContext(ctx, di.grafanaSettings.HTTPMethod, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create new request with context: %w", err)
		}

		req.Header = di.grafanaSettings.CustomHeaders.Clone()

		resp, err = client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to make http request: %w", err)
		}
	}

	// Handle non-200 responses.
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
			// VictoriaLogs returns 422 for query syntax errors with JSON error details.
			return nil, parseErrorResponse(resp.Body)
		case http.StatusBadRequest:
			// VictoriaLogs returns 400 for some errors with plain text messages.
			return nil, parseStringResponseError(resp.Body)
		default:
			return nil, fmt.Errorf("failed to make http request: %d", resp.StatusCode)
		}
	}

	// Handle empty responses (VictoriaLogs returns Content-Length: 0 for no data).
	// This avoids JSON decoding errors when trying to parse an empty body.
	if resp.ContentLength == 0 {
		resp.Body.Close()
		return nil, nil
	}

	return resp.Body, nil
}

// query executes a single query and returns the response as a DataResponse.
// This is the main entry point for non-streaming queries.
//
// The method routes to different response parsers based on QueryType:
//   - QueryTypeStats: Point-in-time statistics (single value)
//   - QueryTypeStatsRange: Time series statistics (multiple values over time)
//   - QueryTypeHits: Histogram of log counts by field values
//   - default (instant): Individual log lines
//
// Each parser returns data in Grafana's frame format for visualization.
func (di *DatasourceInstance) query(ctx context.Context, q *Query) backend.DataResponse {
	// Execute the HTTP request to VictoriaLogs.
	r, err := di.datasourceQuery(ctx, q, false)
	if err != nil {
		return newResponseError(err, backend.StatusInternal)
	}

	if r == nil {
		// VictoriaLogs returned no data (empty response).
		// Return an empty frame list instead of an error.
		return backend.DataResponse{Frames: data.Frames{}}
	}

	// Ensure the response body is closed when we're done.
	defer func() {
		if err := r.Close(); err != nil {
			backend.Logger.Error("failed to close response body", "err", err.Error())
		}
	}()

	// Route to the appropriate parser based on query type.
	switch q.QueryType {
	case QueryTypeStats:
		return parseStatsResponse(r, q)
	case QueryTypeStatsRange:
		return parseStatsResponse(r, q)
	case QueryTypeHits:
		return parseHitsResponse(r)
	default:
		return parseInstantResponse(r)
	}
}

// checkAlertingRequest determines if the request is from Grafana's alerting system.
// Grafana sends a "FromAlert" header with alert queries.
// This is a Grafana-specific convention, not a standard.
//
// Alert queries may need different handling:
//   - Different frame format (numeric multi instead of time series)
//   - Different timeout requirements
//   - Different error handling
func checkAlertingRequest(headers map[string]string) (bool, error) {
	var forAlerting bool
	if val, ok := headers[requestFromAlert]; ok {
		if val == "" {
			return false, nil
		}

		boolValue, err := strconv.ParseBool(val)
		if err != nil {
			return false, fmt.Errorf("failed to parse %s header value: %s", requestFromAlert, val)
		}

		forAlerting = boolValue
	}
	return forAlerting, nil
}

// CheckHealth handles health checks from Grafana.
// This is called when:
//   - User clicks "Save & Test" in the datasource configuration
//   - Grafana periodically checks datasource health
//
// The health check simply requests the /health endpoint from VictoriaLogs
// and verifies it returns HTTP 200. We don't check the response body content
// because VictoriaLogs' /health endpoint just returns "OK" for healthy instances.
//
// Returns:
//   - HealthStatusOk: Connection successful
//   - HealthStatusError: Connection failed (with error message)
func (d *Datasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	res := &backend.CheckHealthResult{}
	di, err := d.getInstance(ctx, req.PluginContext)
	if err != nil {
		res.Status = backend.HealthStatusError
		res.Message = "Error getting datasource instance"
		d.logger.Error("Error getting datasource instance", "err", err)
		return res, nil
	}

	// Build the health check URL.
	// We use strings.TrimRight to handle URLs with or without trailing slashes.
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%s", strings.TrimRight(di.settings.URL, "/"), health), nil)
	if err != nil {
		return newHealthCheckErrorf("could not create request"), nil
	}

	resp, err := di.httpClient.Do(r)
	if err != nil {
		return newHealthCheckErrorf("request error"), nil
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.DefaultLogger.Error("check health: failed to close response body", "err", err.Error())
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return newHealthCheckErrorf("got response code %d", resp.StatusCode), nil
	}

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Data source is working",
	}, nil
}

// RootHandler handles requests to the root path ("/").
// This is mostly for debugging - it returns a simple greeting message.
// In production, this endpoint is rarely used.
func (d *Datasource) RootHandler(rw http.ResponseWriter, req *http.Request) {
	d.logger.Debug("Received resource call", "url", req.URL.String(), "method", req.Method)

	_, err := rw.Write([]byte("Hello from VM data source!"))
	if err != nil {
		d.logger.Warn("Error writing response")
	}

	rw.WriteHeader(http.StatusOK)
}

// VLAPIQuery proxies requests to VictoriaLogs API endpoints that return metadata.
// This is used by the frontend for:
//   - Field value autocomplete in the query editor
//   - Field name suggestions
//   - Stream selection dropdowns
//
// The method:
//  1. Parses the query parameters from the request body
//  2. Builds the VictoriaLogs URL
//  3. Proxies the request to VictoriaLogs
//  4. Streams the response back to the frontend
//
// Endpoints handled: field_values, field_names, streams, stream_field_names, stream_field_values
func (d *Datasource) VLAPIQuery(rw http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	pluginCxt := backend.PluginConfigFromContext(ctx)
	defer func() {
		if err := req.Body.Close(); err != nil {
			d.logger.Error("VLAPIQuery: failed to close request body", "err", err.Error())
		}
	}()

	// Parse the query parameters from the request body.
	fieldsQuery, err := getFieldsQueryFromRaw(req.Body)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}

	di, err := d.getInstance(ctx, pluginCxt)
	if err != nil {
		d.logger.Error("Error loading datasource", "error", err)
		writeError(rw, http.StatusInternalServerError, err)
		return
	}

	// Build the VictoriaLogs URL by combining the base URL with the request path.
	u, err := url.Parse(di.settings.URL)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to parse datasource url: %w", err))
		return
	}
	u.Path = path.Join(u.Path, req.URL.Path)
	u.RawQuery = fieldsQuery.queryParams().Encode()

	newReq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), nil)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to create new request with context: %w", err))
		return
	}

	newReq.Header = di.grafanaSettings.CustomHeaders.Clone()
	resp, err := di.httpClient.Do(newReq)
	if err != nil {
		// Retry once for transient network errors.
		if !isTrivialError(err) {
			writeError(rw, http.StatusBadRequest, err)
			return
		}

		newReq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), nil)
		if err != nil {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to create new request with context: %w", err))
			return
		}

		// Something in the middle between client and datasource might be closing
		// the connection. So we do a one more attempt in hope request will succeed.
		resp, err = di.httpClient.Do(newReq)
		if err != nil {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to make http request: %w", err))
			return
		}
	}
	defer resp.Body.Close()

	// Handle non-200 responses from VictoriaLogs.
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			d.logger.Error("Failed to read response body", "error", err)
			writeError(rw, http.StatusInternalServerError, fmt.Errorf("failed to read response: %w", err))
			return
		}
		d.logger.Error("VictoriaLogs returned error", "status", resp.StatusCode, "body", string(body))
		writeError(rw, resp.StatusCode, fmt.Errorf("VictoriaLogs returned status %d: %s", resp.StatusCode, string(body)))
		return
	}

	// Forward response headers (Content-Type, Content-Encoding) to the client.
	rw.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		rw.Header().Set("Content-Encoding", ce)
	}

	// Stream the response body to the client.
	// Using io.Copy is more efficient than reading into a buffer.
	rw.WriteHeader(http.StatusOK)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		d.logger.Error("Error streaming response", "error", err)
	}
}

// VLAPITenantIDs handles requests for the list of tenant IDs.
// This is used in multitenant VictoriaLogs deployments to populate
// the tenant dropdown in the query editor.
//
// SECURITY NOTE:
// If multitenancy headers (AccountID, ProjectID) are already configured in the
// datasource settings, we remove them from this request. This prevents users
// from querying tenant_ids for a tenant that shouldn't have access to this endpoint.
// This is a security measure for vmauth deployments that enforce tenant access.
//
// See: https://docs.victoriametrics.com/victoriametrics/vmauth/#modifying-http-headers
func (d *Datasource) VLAPITenantIDs(rw http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	pluginCxt := backend.PluginConfigFromContext(ctx)

	di, err := d.getInstance(ctx, pluginCxt)
	if err != nil {
		d.logger.Error("Error loading datasource", "error", err)
		writeError(rw, http.StatusInternalServerError, err)
		return
	}

	// Return a hint if URL is not configured yet.
	// This helps users understand what they need to do before this endpoint works.
	if di.settings.URL == "" {
		rw.Header().Add("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, err = rw.Write([]byte(`{"hint": "To use the list of possible tenants, need to set the datasource url first and save the datasource configuration"}`))
		return
	}

	u, err := url.Parse(di.settings.URL)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to parse datasource url: %w", err))
		return
	}
	u.Path = path.Join(u.Path, req.URL.Path)
	newReq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), nil)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to create new request with context: %w", err))
		return
	}

	newReq.Header = di.grafanaSettings.CustomHeaders.Clone()

	// SECURITY: Remove multitenancy headers to prevent unauthorized tenant_id access.
	// If vmauth is configured to restrict access based on tenant headers,
	// removing these allows vmauth to enforce the correct permissions.
	newReq.Header.Del(accountIDHeader)
	newReq.Header.Del(projectIDHeader)

	resp, err := di.httpClient.Do(newReq)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to make http request: %w", err))
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("failed to read http response body: %w", err))
		return
	}

	rw.Header().Add("Content-Type", "application/json")

	// BACKWARD COMPATIBILITY: VictoriaLogs versions before 1.38.0 don't support
	// the /select/tenant_ids endpoint. They return an error message with 200 status.
	// We detect this and return an empty object instead of propagating the error.
	if bytes.Contains(bodyBytes, []byte("unsupported path requested:")) {
		rw.WriteHeader(http.StatusOK)
		_, err = rw.Write([]byte(`{}`))
		return
	}

	rw.WriteHeader(http.StatusOK)
	_, err = rw.Write(bodyBytes)
	if err != nil {
		log.DefaultLogger.Warn("Error writing response")
	}
}

// VMUIQuery returns the URL and tenant information for VictoriaLogs' native UI (VMUI).
// The frontend uses this to provide an "Open in VMUI" button that opens a new tab
// with the native VictoriaLogs query interface.
//
// This is useful because VMUI provides features not available in Grafana's logs panel,
// such as advanced query debugging and log exploration tools.
func (d *Datasource) VMUIQuery(rw http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	pluginCxt := backend.PluginConfigFromContext(ctx)

	di, err := d.getInstance(ctx, pluginCxt)
	if err != nil {
		d.logger.Error("Error loading datasource", "error", err)
		writeError(rw, http.StatusInternalServerError, err)
		return
	}

	// Get the VMUI URL (either configured or derived from datasource URL).
	vmuiUrl, err := getBaseVMUIURL(di.settings)
	if err != nil {
		d.logger.Error("failed to build VMUI url", "error", err)
		writeError(rw, http.StatusInternalServerError, err)
		return
	}

	// Get tenant information for the URL.
	// Default to "0" if not configured (single-tenant deployments).
	accountID := di.grafanaSettings.MultitenancyHeaders.AccountID
	if accountID == "" {
		accountID = "0"
	}

	projectID := di.grafanaSettings.MultitenancyHeaders.ProjectID
	if projectID == "" {
		projectID = "0"
	}

	rw.Header().Add("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)

	// Return VMUI URL and tenant info as JSON.
	if _, err := fmt.Fprintf(rw, `{"vmuiURL": %q, "accountID": %q, "projectID": %q}`,
		vmuiUrl, accountID, projectID); err != nil {
		d.logger.Warn("Error writing response", "error", err)
	}
}

// getBaseVMUIURL returns the VMUI URL for this datasource.
// If explicitly configured, uses that. Otherwise, derives it from the base URL.
func getBaseVMUIURL(settings DataSourceInstanceSettings) (string, error) {
	if len(settings.VMUIURL) > 0 {
		return settings.VMUIURL, nil
	}

	if len(settings.URL) == 0 {
		return "", fmt.Errorf("data source URL is not set")
	}

	// Derive VMUI URL by appending "/select/vmui/" to the base URL.
	vmuiUrl, err := newURL(settings.URL, "/select/vmui/", false)
	if err != nil {
		return "", err
	}

	return vmuiUrl.String(), nil
}

// newURL constructs a URL by parsing the base URL and appending a path.
// This is a helper for building VictoriaLogs API URLs.
//
// Parameters:
//   - urlStr: Base URL of the VictoriaLogs server
//   - p: Path to append
//   - root: If true, truncates the path at "/select/" before appending (for root-level endpoints)
func newURL(urlStr, p string, root bool) (*url.URL, error) {
	if urlStr == "" {
		return nil, fmt.Errorf("url can't be blank")
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse datasource url: %s", err)
	}
	if root {
		// For root-level endpoints, remove any existing "/select/..." path.
		// This is needed when the base URL includes the select path prefix.
		if idx := strings.Index(u.Path, "/select/"); idx > 0 {
			u.Path = u.Path[:idx]
		}
	}
	u.Path = path.Join(u.Path, p)
	return u, nil
}

// newHealthCheckErrorf creates a health check result with error status.
// This is a convenience function for consistent error formatting.
func newHealthCheckErrorf(format string, args ...interface{}) *backend.CheckHealthResult {
	return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: fmt.Sprintf(format, args...)}
}

// newResponseError creates a DataResponse with an error.
// This logs the error and returns it in a format Grafana can display.
func newResponseError(err error, httpStatus backend.Status) backend.DataResponse {
	log.DefaultLogger.Error(err.Error())
	return backend.DataResponse{Status: httpStatus, Error: err}
}

// isTrivialError determines if an error is transient and worth retrying.
// These errors often occur when intermediate proxies or load balancers
// close idle connections.
//
// Trivial errors include:
//   - EOF (connection closed unexpectedly)
//   - Unexpected EOF (partial read)
//   - "broken pipe" (write to closed connection)
//   - "reset by peer" (connection forcibly closed)
func isTrivialError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Suppress trivial network errors, which could occur at remote side.
	s := err.Error()
	if strings.Contains(s, "broken pipe") || strings.Contains(s, "reset by peer") {
		return true
	}
	return false
}

// parseCustomHeaders extracts custom HTTP headers from the datasource configuration.
// Grafana stores header names in JSONData and header values in DecryptedSecureJSONData
// (for security, sensitive values like API keys are encrypted in the database).
//
// The naming convention is:
//   - JSONData: {"httpHeaderName1": "Authorization", "httpHeaderName2": "X-API-Key"}
//   - SecureJSONData: {"httpHeaderValue1": "Bearer token123", "httpHeaderValue2": "secret-key"}
//
// This function pairs them up and returns an http.Header map.
func parseCustomHeaders(jsonData json.RawMessage, decryptedSecureJSONData map[string]string) (http.Header, error) {
	var headersSettings map[string]json.RawMessage
	if err := json.Unmarshal(jsonData, &headersSettings); err != nil {
		return nil, fmt.Errorf("failed to parse datasource settings: %w", err)
	}

	headers := http.Header{}
	for k, v := range headersSettings {
		// Only process keys that start with "httpHeaderName"
		if !strings.HasPrefix(k, httpHeaderName) {
			continue
		}
		var headerName string
		if err := json.Unmarshal(v, &headerName); err != nil {
			return nil, fmt.Errorf("failed to parse header value: %w", err)
		}
		// Skip empty header names
		if len(headerName) == 0 {
			continue
		}
		// Convert "httpHeaderName1" to "httpHeaderValue1" to find the corresponding value
		headerValueName := strings.Replace(k, httpHeaderName, httpHeaderValue, 1)
		if headerValue, ok := decryptedSecureJSONData[headerValueName]; ok {
			headers.Add(headerName, headerValue)
		}
	}
	return headers, nil
}

// writeError writes a JSON error response to the HTTP response writer.
// This is used for resource API endpoints to return errors in a consistent format.
//
// Response format: {"error": "Internal Server Error", "message": "actual error message"}
func writeError(rw http.ResponseWriter, statusCode int, err error) {
	d := make(map[string]interface{})

	d["error"] = "Internal Server Error"
	d["message"] = err.Error()

	var b []byte
	if b, err = json.Marshal(d); err != nil {
		rw.WriteHeader(statusCode)
		return
	}

	rw.Header().Add("Content-Type", "application/json")
	rw.WriteHeader(http.StatusInternalServerError)

	_, err = rw.Write(b)
	if err != nil {
		log.DefaultLogger.Warn("Error writing response")
	}
}

// buildDatasourceSettings extracts VictoriaLogs-specific settings from Grafana's config.
// This parses the JSONData field which contains user-configured options.
func buildDatasourceSettings(settings backend.DataSourceInstanceSettings) (DataSourceInstanceSettings, error) {
	var dstSettings DataSourceInstanceSettings

	// Parse the VMUIURL from the settings (if configured)
	if err := json.Unmarshal(settings.JSONData, &dstSettings); err != nil {
		return dstSettings, fmt.Errorf("failed to parse datasource JSONData settings: %w", err)
	}
	// The URL comes from a separate field in Grafana's settings, not JSONData
	dstSettings.URL = settings.URL

	return dstSettings, nil
}

// parseMultitenancyHeaders extracts multitenancy configuration from datasource settings.
// This is used for VictoriaLogs clusters that have multiple tenants, where each tenant's
// data is identified by AccountID and ProjectID headers.
//
// Default values:
//   - AccountID: "0" (single-tenant mode)
//   - ProjectID: "0" (no project subdivision)
func parseMultitenancyHeaders(settings backend.DataSourceInstanceSettings) (MultitenancyHeaders, error) {
	defaults := MultitenancyHeaders{
		AccountID: "0",
		ProjectID: "0",
	}

	var config struct {
		Headers map[string]interface{} `json:"multitenancyHeaders"`
	}
	if err := json.Unmarshal(settings.JSONData, &config); err != nil {
		return defaults, err
	}

	if config.Headers == nil {
		return defaults, nil
	}

	result := defaults
	// Helper function to parse a tenant ID from the config
	setTenant := func(key string, target *string) error {
		if val, ok := config.Headers[key]; ok {
			parsed, err := parseTenantId(val)
			if err != nil {
				return err
			}
			*target = parsed
		}
		return nil
	}

	if err := setTenant(accountIDHeader, &result.AccountID); err != nil {
		return defaults, err
	}
	if err := setTenant(projectIDHeader, &result.ProjectID); err != nil {
		return defaults, err
	}

	return result, nil
}

// parseTenantId converts a tenant ID from various types to a string.
// The frontend may send tenant IDs as strings or numbers, so we handle both.
//
// Valid formats:
//   - string: "123" (validated as numeric)
//   - float64: 123.0 (from JSON number parsing)
//   - other: converted via fmt.Sprintf
func parseTenantId(tenantId interface{}) (string, error) {
	switch val := tenantId.(type) {
	case string:
		// Validate that the string is a valid number
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return "", fmt.Errorf("tenant ID is not a number: %w", err)
		}
		return val, nil
	case float64:
		// JSON numbers are parsed as float64
		return strconv.FormatInt(int64(val), 10), nil
	default:
		// Fallback for unexpected types
		return fmt.Sprintf("%v", val), nil
	}
}

// setVmuiURL sets the VMUI URL if not explicitly configured.
// It derives the URL from the datasource URL by appending "/select/vmui/".
// This allows the "Open in VMUI" feature to work without manual configuration.
func setVmuiURL(settings *DataSourceInstanceSettings) error {
	if len(settings.VMUIURL) == 0 && settings.URL != "" {
		vmuiUrl, err := newURL(settings.URL, "/select/vmui/", false)
		if err != nil {
			return fmt.Errorf("failed to build VMUI url: %w", err)
		}
		settings.VMUIURL = vmuiUrl.String()
	}

	return nil
}
