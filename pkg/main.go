// Package main - Plugin Entry Point
//
// This is the main entry point for the VictoriaLogs Grafana datasource backend plugin.
// Every Grafana datasource backend plugin must export a `main` function that:
// - Registers with Grafana's plugin system
// - Sets up HTTP server for routing
// - Handles the plugin lifecycle
//
// The plugin binary is compiled separately and runs as a standalone process managed by Grafana.
// It implements four handler interfaces:
// - QueryDataHandler: Executes LogsQL queries and returns data frames
// - CallResourceHandler: HTTP mux for resource API (field names, field values, tenant IDs, VMUI URL)
// - CheckHealthHandler: Verifies VictoriaLogs is reachable via /health
// - StreamHandler: Manages live tail connections via /select/logsql/tail
//
// ARCHITECTURE:
// The backend runs as a separate process from the frontend (TypeScript/React).
// Communication happens through Grafana's plugin protocol (gRPC over HTTP).
// The backend plugin:
// 1. Receives query requests from frontend (via DataSourceWithBackend)
// 2. Routes to appropriate VictoriaLogs endpoint based on QueryType
// 3. Builds HTTP requests to VictoriaLogs
// 4. Parses responses (NDJSON, JSON) and returns data frames to frontend
// 5. Handles streaming responses for live tail
//
// WHY a backend plugin?
// Grafana datasource plugins can be frontend-only (browser → VictoriaLogs directly) or have a backend component. This plugin uses a Go backend so that:
// 1. VictoriaLogs credentials stay on the server (never exposed to the browser)
// 2. The server can proxy and transform responses (NDJSON parsing, alerting support)
// 3. Live streaming works through Grafana's server-side streaming infrastructure
// 4. Alerting rules can execute queries without a browser session
//
// For more details on the plugin architecture, see:
// - onboarding/onboarding-system-overview.md
// - onboarding/onboarding-backend.md

package main

import (
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/VictoriaMetrics/victorialogs-datasource/pkg/plugin"
)

// VL_PLUGIN_ID describes plugin name that matches Grafana plugin naming convention
const VL_PLUGIN_ID = "victoriametrics-logs-datasource"

func main() {
	// Setup the plugin environment before initializing
	// This configures logging, plugin metadata for and other SDK components
	backend.SetupPluginEnvironment(VL_PLUGIN_ID)

	// Create a logger instance for logging output
	pluginLogger := log.New()

	// Create the datasource instance
	// This is the main object that handles all plugin functionality
	ds := plugin.NewDatasource()

	// Log that the plugin is starting
	pluginLogger.Info("Starting VL datasource")

	// Register the plugin with Grafana.
	// backend.Manage is the core function that starts the plugin process
	// and registers all the handlers that Grafana will call.
	//
	// The Manage function:
	// - Creates the plugin process
	// - Registers handlers for different types of requests
	// - Blocks until the process is terminated
	//
	// Handler interfaces (all implemented by our Datasource struct):
	// CallResourceHandler: Handles HTTP requests for field names, field values, etc.
	// QueryDataHandler: Executes LogsQL queries and returns data frames
	// CheckHealthHandler: Health check endpoint
	// StreamHandler: Live log tail streaming
	err := backend.Manage(VL_PLUGIN_ID, backend.ServeOpts{
		CallResourceHandler: ds, // Resource API (field names, tenants, etc.)
		QueryDataHandler:    ds, // Main query execution
		CheckHealthHandler:  ds, // Health check endpoint
		StreamHandler:       ds, // Live log streaming
	})
	if err != nil {
		// Log the error and exit if plugin fails to start
		pluginLogger.Error("Error starting VL datasource", "error", err.Error())
	}
}
