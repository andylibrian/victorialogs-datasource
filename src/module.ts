/**
 * Plugin Entry Point - Module Registration
 *
 * This is the main entry point for the VictoriaLogs Grafana datasource plugin.
 * Every Grafana datasource plugin must export a `plugin` constant that registers
 * the datasource with Grafana's plugin system.
 *
 * ARCHITECTURE NOTE:
 * Grafana datasource plugins follow a specific registration pattern where we
 * tell Grafana which classes to use for different aspects of the plugin:
 * - The datasource class handles query execution and data fetching
 * - The query editor provides the UI for building queries
 * - The config editor provides the datasource settings UI
 *
 * WHY THIS MATTERS:
 * This registration happens once when Grafana loads the plugin. The classes
 * registered here are instantiated by Grafana as needed. This is the bridge
 * between Grafana's core and our custom VictoriaLogs functionality.
 *
 * For more details on the plugin architecture, see:
 * - onboarding/onboarding-system-overview.md
 * - onboarding/onboarding-frontend.md
 */

import { DataSourcePlugin } from '@grafana/data';

import QueryEditorByApp from './components/QueryEditor/QueryEditorByApp';
import ConfigEditor from './configuration/ConfigEditor';
import { VictoriaLogsDatasource } from './datasource';

/**
 * Plugin Registration
 *
 * This creates and exports the plugin instance that Grafana will use.
 * We use the builder pattern to configure the plugin with our custom components:
 *
 * 1. VictoriaLogsDatasource: The main datasource class that handles:
 *    - Query execution and transformation
 *    - Template variable interpolation
 *    - Communication with the Go backend
 *    - Response post-processing
 *
 * 2. QueryEditorByApp: A smart query editor component that:
 *    - Routes to the appropriate editor based on context (Explore vs Dashboard)
 *    - Provides both Code mode (raw LogsQL) and Builder mode (visual)
 *    - Handles query validation and hints
 *
 * 3. ConfigEditor: The datasource configuration UI that:
 *    - Sets up VictoriaLogs connection URL
 *    - Configures authentication and headers
 *    - Manages derived fields and log level rules
 *    - Sets up multi-tenancy if needed
 *
 * The DataSourcePlugin base class from Grafana handles all the plugin protocol
 * communication, so we just need to provide our custom implementations.
 */
export const plugin = new DataSourcePlugin(VictoriaLogsDatasource)
  .setQueryEditor(QueryEditorByApp)
  .setConfigEditor(ConfigEditor);
