/**
 * VictoriaLogs Datasource - Core Query Execution and Data Management
 *
 * This is the main datasource class for the VictoriaLogs Grafana plugin. It serves as the
 * primary interface between Grafana and VictoriaLogs, handling all aspects of query execution,
 * data transformation, and integration with Grafana's features.
 *
 * ARCHITECTURE OVERVIEW:
 * This class extends DataSourceWithBackend, which means:
 * - It delegates actual HTTP communication to a Go backend plugin (see pkg/plugin/datasource.go)
 * - The frontend handles query manipulation, variable interpolation, and response post-processing
 * - The backend handles HTTP requests to VictoriaLogs and NDJSON parsing
 *
 * WHY THIS DESIGN?
 * 1. Security: VictoriaLogs credentials stay on the server, never exposed to the browser
 * 2. Performance: Go backend can efficiently parse large NDJSON streams
 * 3. Alerting: Backend can execute queries without a browser session
 * 4. Streaming: Live tail works through Grafana's server-side streaming infrastructure
 *
 * KEY RESPONSIBILITIES:
 * 1. Query Execution: Transform user queries and send to backend
 * 2. Variable Interpolation: Replace template variables with actual values
 * 3. Response Transformation: Enrich data frames with derived fields and log levels
 * 4. Ad-hoc Filters: Support Grafana's ad-hoc filtering feature
 * 5. Template Variables: Provide field names/values for variable queries
 * 6. Log Context: Implement "Show logs context" feature in Explore
 * 7. Live Streaming: Support real-time log tailing
 *
 * DATA FLOW:
 * User Query → query() → applyTemplateVariables() → Backend → transformBackendResult() → Grafana
 *
 * For detailed flow documentation, see onboarding/onboarding-query-flow.md
 */

import { cloneDeep } from 'lodash';
import { lastValueFrom, map, merge, Observable } from 'rxjs';

import {
  AdHocVariableFilter,
  CoreApp,
  DataFrame,
  DataQueryRequest,
  DataQueryResponse,
  DataSourceGetTagKeysOptions,
  DataSourceGetTagValuesOptions,
  DataSourceInstanceSettings,
  DataSourceWithLogsContextSupport,
  DEFAULT_FIELD_DISPLAY_VALUES_LIMIT,
  Labels,
  LegacyMetricFindQueryOptions,
  LiveChannelScope,
  LoadingState,
  LogRowContextOptions,
  LogRowContextQueryDirection,
  LogRowModel,
  MetricFindValue,
  QueryVariableModel,
  rangeUtil,
  ScopedVars,
  SupplementaryQueryOptions,
  SupplementaryQueryType,
  TimeRange,
  toUtc,
  TypedVariableModel,
} from '@grafana/data';
import { config, DataSourceWithBackend, getGrafanaLiveSrv, getTemplateSrv, TemplateSrv } from '@grafana/runtime';

// LogsQL query manipulation utilities
// These handle transforming LogsQL expressions for various scenarios
import { correctMultiExactOperatorValueAll } from './LogsQL/multiExactOperator';
import { correctRegExpValueAll, doubleQuoteRegExp, isRegExpOperatorInLastFilter } from './LogsQL/regExpOperator';

// UI Components
import QueryEditor from './components/QueryEditor/QueryEditor';
import { LogLevelRule } from './configuration/LogLevelRules/types';

// Constants and utilities
import { TEXT_FILTER_ALL_VALUE, VARIABLE_ALL_VALUE } from './constants';
import { escapeLabelValueInSelector } from './languageUtils';
import LogsQlLanguageProvider from './language_provider';
import { LOGS_VOLUME_BARS, queryLogsVolume } from './logsVolumeLegacy';

// Query manipulation functions
// These functions handle adding/removing filters and pipes to LogsQL queries
import {
  addLabelToQuery,
  addSortPipeToQuery,
  getQueryFormat,
  queryHasFilter,
  removeLabelFromQuery,
} from './modifyQuery';

// Variable interpolation utilities
// These handle the complex logic of replacing template variables in queries
import { removeDoubleQuotesAroundVar } from './parsing';
import { replaceOperatorWithIn, returnVariables } from './parsingUtils';

// Response transformation
// This enriches backend responses with derived fields and log levels
import { transformBackendResult } from './transformers';

// Type definitions
import {
  DerivedFieldConfig,
  FilterActionType,
  FilterFieldType,
  MultitenancyHeaders,
  Options,
  Query,
  QueryBuilderLimits,
  QueryFilterOptions,
  QueryType,
  SupportingQueryType,
  Tenant,
  TenantHeaderNames,
  ToggleFilterAction,
  VariableQuery,
} from './types';

// Time utilities for timezone handling
import { formatOffsetDuration, getMillisecondsFromDuration } from './utils/timeUtils';

// Variable support for template variables
import { VariableSupport } from './variableSupport/VariableSupport';

/**
 * RefID prefixes for supplementary queries
 *
 * Grafana uses RefID to identify queries. We use these prefixes to mark
 * queries that are created automatically by the plugin (not by the user):
 *
 * - LOG_VOLUME: For the logs volume histogram shown above logs in Explore
 * - LOG_SAMPLE: For sampling logs when showing logs volume
 * - LOG_CONTEXT: For "Show context" queries that fetch surrounding logs
 *
 * WHY PREFIXES?
 * These prefixes help us identify supplementary queries in the response
 * transformation pipeline and handle them differently from user queries.
 */
export const REF_ID_STARTER_LOG_VOLUME = 'log-volume-';
export const REF_ID_STARTER_LOG_SAMPLE = 'log-sample-';
export const REF_ID_STARTER_LOG_CONTEXT_REQUEST = 'log-context-request-';
export const REF_ID_STARTER_LOG_CONTEXT_QUERY = 'log-context-query-';

/**
 * Special field name for stream identification
 *
 * VictoriaLogs uses _stream_id to uniquely identify log streams.
 * This is used in log context queries to fetch logs from the same stream.
 */
export const LABEL_STREAM_ID = '_stream_id';

/**
 * VictoriaLogsDatasource - Main Datasource Class
 *
 * This is the core class that implements Grafana's datasource interface for VictoriaLogs.
 * It extends DataSourceWithBackend, which provides the base implementation for communicating
 * with a backend plugin (the Go code in pkg/plugin/).
 *
 * KEY INTERFACES IMPLEMENTED:
 * - DataSourceApi: Standard Grafana datasource interface (query, filterQuery, etc.)
 * - DataSourceWithLogsContextSupport: Adds support for "Show logs context" in Explore
 *
 * LIFECYCLE:
 * 1. Grafana instantiates this class once per datasource instance (per datasource ID)
 * 2. Constructor reads settings from Grafana (URL, auth, derived fields, etc.)
 * 3. query() is called whenever a user runs a query or dashboard refreshes
 * 4. Instance is disposed when datasource is deleted or settings change
 *
 * THREAD SAFETY:
 * Each datasource instance is shared across all panels/queries using that datasource.
 * The class must be stateless or use thread-safe patterns for mutable state.
 */
export class VictoriaLogsDatasource
  extends DataSourceWithBackend<Query, Options>
  implements DataSourceWithLogsContextSupport
{
  // Instance properties - set once in constructor and never modified
  id: number;
  uid: string;
  url: string;
  maxLines: number;
  derivedFields: DerivedFieldConfig[];
  basicAuth?: string;
  withCredentials?: boolean;
  httpMethod: string;
  customQueryParameters: URLSearchParams;
  languageProvider?: LogsQlLanguageProvider;
  queryBuilderLimits?: QueryBuilderLimits;
  logLevelRules: LogLevelRule[];
  multitenancyHeaders?: MultitenancyHeaders;

  /**
   * Constructor - Initialize Datasource Instance
   *
   * Called by Grafana when creating a new datasource instance. This happens:
   * - Once when Grafana loads (for each configured datasource)
   * - Again when datasource settings are changed (old instance disposed, new one created)
   *
   * @param instanceSettings - Configuration from Grafana (URL, auth, JSON settings)
   * @param templateSrv - Template service for variable interpolation (injected for testing)
   * @param languageProvider - Autocomplete provider (injected for testing)
   */
  constructor(
    instanceSettings: DataSourceInstanceSettings<Options>,
    private readonly templateSrv: TemplateSrv = getTemplateSrv(),
    languageProvider?: LogsQlLanguageProvider
  ) {
    super(instanceSettings);

    // Extract configuration from Grafana settings
    // jsonData contains plugin-specific settings from the config UI
    const settingsData = instanceSettings.jsonData || {};

    // Basic datasource configuration
    this.id = instanceSettings.id;
    this.uid = instanceSettings.uid;
    this.url = instanceSettings.url!;
    this.basicAuth = instanceSettings.basicAuth;
    this.withCredentials = instanceSettings.withCredentials;

    // HTTP configuration
    // POST is preferred for large queries, but GET is supported for compatibility
    this.httpMethod = settingsData.httpMethod || 'POST';

    // Query limits
    // maxLines limits the number of log lines returned (default: 1000)
    // This prevents browser memory issues with huge result sets
    this.maxLines = parseInt(settingsData.maxLines ?? '0', 10) || 1000;

    // Derived fields configuration
    // These extract patterns from log lines (e.g., trace IDs, URLs)
    // and create clickable links to other systems
    this.derivedFields = settingsData.derivedFields || [];

    // Custom query parameters
    // These are appended to every VictoriaLogs API request
    // Useful for things like tenant IDs or custom filters
    this.customQueryParameters = new URLSearchParams(settingsData.customQueryParameters);

    // Language provider for autocomplete
    // Provides field names and values for the query editor
    this.languageProvider = languageProvider ?? new LogsQlLanguageProvider(this);

    // Annotation query editor
    // This allows using the query editor for Grafana annotations
    this.annotations = {
      QueryEditor: QueryEditor,
    };

    // Variable support
    // Handles template variable queries (field names, field values)
    this.variables = new VariableSupport(this);

    // Query builder limits
    // Prevents autocomplete from returning too many results
    this.queryBuilderLimits = settingsData.queryBuilderLimits;

    // Log level detection rules
    // Custom rules for extracting log levels from fields
    this.logLevelRules = settingsData.logLevelRules || [];

    // Multi-tenancy headers
    // Used for VictoriaLogs cluster multi-tenancy (AccountID:ProjectID)
    this.multitenancyHeaders = this.parseMultitenancyHeaders(settingsData.multitenancyHeaders);
  }

  /**
   * query - Main Query Execution Method
   * 
   * This is the primary method called by Grafana when a query needs to be executed.
   * It's called from:
   * - Dashboard panels when they refresh
   * - Explore view when user runs a query
   * - Alerting system when evaluating alert rules
   * 
   * RESPONSIBILITIES:
   * 1. Filter out hidden or empty queries
   * 2. Add sort pipes for proper log ordering
   * 3. Apply timezone offset for time-based queries
   * 4. Detect query format (logs vs histogram)
   * 5. Interpolate template variables in step parameter
   * 6. Route to either live streaming or regular query
   * 
   * @param request - Contains queries, time range, and other context
   * @returns Observable that emits query responses (supports streaming for live tail)
   */
  query(request: DataQueryRequest<Query>): Observable<DataQueryResponse> {
    // Calculate timezone offset for VictoriaLogs queries
    // This ensures time-based queries respect the dashboard's timezone
    const timezoneOffset = formatOffsetDuration(request.timezone, request.range.from.utcOffset());
    
    // Process each query in the request
    // A request can contain multiple queries (multiple panels, mixed datasources)
    const queries = request.targets
      .filter((q) => q.expr || config.publicDashboardAccessToken !== '')
      .map((q) => {
        return {
          ...q,
          // Add sort pipe for log ordering
          // WHY: VictoriaLogs doesn't guarantee order, so we add "sort by (_time) desc/asc"
          // This ensures logs appear in the expected order in the UI
          // The sort is added in the frontend because it's a UI concern, not a data concern
          expr: addSortPipeToQuery(q, request.app, request.liveStreaming),
          
          // Apply maxLines limit (user override or datasource default)
          maxLines: q.maxLines ?? this.maxLines,
          
          // Pass timezone offset to backend
          timezoneOffset,
          
          // Detect if this is a histogram query
          // WHY: Histogram queries need special handling in the backend
          // This detection happens here because it's based on LogsQL syntax
          format: getQueryFormat(q.expr),
          
          // Interpolate template variables in step parameter
          // Example: "$interval" → "5m"
          step: this.templateSrv.replace(q.step, request.scopedVars),
        };
      });

    // Adjust interval if step is explicitly defined
    // WHY: For stats queries, the step determines bar width in charts
    // If user specifies a step, we use it as the interval for proper visualization
    request.intervalMs = queries[0]?.step ? getMillisecondsFromDuration(queries[0]?.step) : request.intervalMs;
    request.targets = queries;

    // Route to live streaming or regular query
    // Live streaming is used for "Live tail" in Explore view
    if (request.liveStreaming) {
      return this.runLiveQueryThroughBackend(request);
    }

    // Regular query through backend
    // This calls DataSourceWithBackend.query() which communicates with the Go backend
    return this.runQuery(request);
  }

  /**
   * runQuery - Execute Query and Transform Response
   * 
   * Wraps the parent query method to add response transformation.
   * The transformation enriches the response with:
   * - Derived fields (clickable links extracted from log lines)
   * - Log levels (info, error, debug, etc.)
   * - Label formatting for better display
   * 
   * @param fixedRequest - The query request with all variables interpolated
   * @returns Observable that emits transformed query responses
   */
  runQuery(fixedRequest: DataQueryRequest<Query>) {
    return super
      .query(fixedRequest)
      .pipe(
        map((response) => transformBackendResult(
          response,
          fixedRequest,
          this.derivedFields ?? [],
          this.getActiveLevelRules()
        ))
      );
  }

    return this.runQuery(request);
  }

  runQuery(fixedRequest: DataQueryRequest<Query>) {
    return super
      .query(fixedRequest)
      .pipe(
        map((response) =>
          transformBackendResult(response, fixedRequest, this.derivedFields ?? [], this.getActiveLevelRules())
        )
      );
  }

  /**
   * toggleQueryFilter - Toggle a Filter On/Off
   * 
   * This method is called when a user clicks on a log label to filter by that label.
   * It implements the "Filter for" and "Filter out" functionality in the logs UI.
   * 
   * BEHAVIOR:
   * - If the filter already exists: Remove it (toggle off)
   * - If "Filter for": Add a positive filter (field:value)
   * - If "Filter out": Add a negative filter (field!=value)
   * 
   * WHY THIS MATTERS:
   * This is a core UX feature for exploring logs. Users can quickly drill down
   * into specific log patterns without manually editing the LogsQL query.
   * 
   * @param query - The current query object
   * @param filter - Contains the filter action and options (key, value, type)
   * @returns Modified query with the filter toggled
   */
  toggleQueryFilter(query: Query, filter: ToggleFilterAction): Query {
    let expression = query.expr ?? '';

    // Validate that we have a key and value to filter on
    if (!filter.options?.key || !filter.options?.value) {
      return { ...query, expr: expression };
    }

    // Escape special characters in the filter value
    // This prevents LogsQL injection and syntax errors
    const value = escapeLabelValueInSelector(filter.options.value);
    
    // Check if this filter already exists in the query
    const hasFilter = queryHasFilter(expression, filter.options.key, value);

    // If filter exists, remove it (toggle behavior)
    if (hasFilter) {
      expression = removeLabelFromQuery(expression, filter.options.key, value);
    }

    const isFilterFor = filter.type === FilterActionType.FILTER_FOR;
    const isFilterOut = filter.type === FilterActionType.FILTER_OUT;

    // Add filter if it doesn't exist (for "Filter for") or always for "Filter out"
    if ((isFilterFor && !hasFilter) || isFilterOut) {
      const operator = isFilterFor ? '=' : '!=';
      expression = addLabelToQuery(expression, { key: filter.options.key, value, operator });
    }

    return { ...query, expr: expression };
  }

  /**
   * queryHasFilter - Check if a Query Contains a Specific Filter
   * 
   * Used to determine if a filter is already present in the query.
   * This is useful for UI state (showing toggle state) and preventing duplicates.
   * 
   * @param query - The query to check
   * @param filter - The filter to look for (key, value)
   * @returns true if the filter exists in the query
   */
  queryHasFilter(query: Query, filter: QueryFilterOptions): boolean {
    const expression = query.expr ?? '';
    return queryHasFilter(expression, filter.key, filter.value, '=');
  }

  /**
   * filterQuery - Determine if a Query Should Be Executed
   * 
   * Grafana calls this method for each query before execution.
   * Returning false prevents the query from being sent to the backend.
   * 
   * FILTERING CRITERIA:
   * - Queries with hide=true are skipped (user disabled them)
   * - Queries with empty expressions are skipped (nothing to query)
   * 
   * @param query - The query to evaluate
   * @returns true if the query should be executed
   */
  filterQuery(query: Query): boolean {
    if (query.hide || query.expr === '') {
      return false;
    }
    return true;
  }

  /**
   * applyTemplateVariables - Interpolate Template Variables in Queries
   * 
   * This method is called by Grafana before executing a query. It replaces
   * template variables (like $var, ${var}, [[var]]) with their actual values.
   * 
   * TEMPLATE VARIABLES IN GRAFANA:
   * Template variables allow users to create dynamic dashboards. For example:
   * - $environment → "production"
   * - $server → "server1,server2,server3"
   * 
   * WHAT THIS METHOD DOES:
   * 1. Removes built-in Grafana variables that shouldn't be interpolated here
   * 2. Replaces standard variables with their interpolated versions
   * 3. Applies ad-hoc filters (filters added via UI dropdowns)
   * 4. Handles extra filters from query options
   * 
   * WHY CUSTOM IMPLEMENTATION?
   * We need special handling for:
   * - __interval and __interval_ms: These are replaced with literal strings
   *   because they need to be interpolated by VictoriaLogs, not Grafana
   * - Multi-value variables: Need to be converted to LogsQL "in()" syntax
   * - Ad-hoc filters: Need to be added to the query expression
   * 
   * @param target - The query object with template variables
   * @param scopedVars - Variables available in this context
   * @param adhocFilters - Filters added via Grafana's ad-hoc filter UI
   * @returns Query with all variables replaced
   */
  applyTemplateVariables(target: Query, scopedVars: ScopedVars, adhocFilters?: AdHocVariableFilter[]): Query {
    // Remove built-in variables that we handle specially
    // We keep __interval and __interval_ms but replace them with literal strings
    // This allows VictoriaLogs to handle time-based aggregations
    const { __auto, __interval, __interval_ms, __range, __range_s, __range_ms, ...rest } = scopedVars || {};

    const variables = {
      ...rest,
      // Replace with literal strings so VictoriaLogs can interpret them
      // Example: $__interval becomes the string "$__interval" in the query
      // VictoriaLogs then replaces it with the actual interval value
      __interval: {
        value: '$__interval',
      },
      __interval_ms: {
        value: '$__interval_ms',
      },
    };

    // Get extra filters (ad-hoc filters that apply to all queries)
    let extraFilters = this.getExtraFilters(adhocFilters, target.extraFilters);
    
    // Interpolate variables in the query expression
    let expr = this.interpolateString(target.expr, variables);
    
    // Apply extra filters to root query if configured
    // This prepends filters like "environment:production" before the main query
    if (target.isApplyExtraFiltersToRootQuery && extraFilters) {
      expr = `${extraFilters} | ${expr}`;
      extraFilters = undefined;
    }

    return {
      ...target,
      // Interpolate legend format (used in chart labels)
      legendFormat: this.templateSrv.replace(target.legendFormat, rest),
      expr,
      extraFilters,
    };
  }

  /**
   * getExtraFilters - Build Filter Expression from Ad-hoc Filters
   * 
   * Converts Grafana ad-hoc filters into LogsQL filter expressions.
   * Ad-hoc filters are the dropdown filters shown at the top of dashboards.
   * 
   * @param adhocFilters - Array of ad-hoc filters from Grafana
   * @param initialExpr - Existing filter expression to append to
   * @returns LogsQL filter expression or undefined
   */
  getExtraFilters(adhocFilters?: AdHocVariableFilter[], initialExpr = ''): string | undefined {
    if (!adhocFilters) {
      return initialExpr || undefined;
    }

    // Build filter expression by reducing all ad-hoc filters
    const expr = adhocFilters.reduce((acc: string, filter: AdHocVariableFilter) => {
      return addLabelToQuery(acc, filter);
    }, initialExpr);

    // Return variables removes the special encoding we use for multi-value variables
    return returnVariables(expr);
  }

  /**
   * interpolateQueryExpr - Custom Variable Interpolation for Query Expressions
   * 
   * This method provides custom interpolation logic for multi-value template variables.
   * It's called by Grafana's template service when replacing variables in queries.
   * 
   * MULTI-VALUE VARIABLE HANDLING:
   * When a template variable has multiple values (e.g., ["app1", "app2", "app3"]),
   * we can't simply replace it with a comma-separated list because LogsQL
   * requires a special syntax for multi-value filters.
   * 
   * APPROACH:
   * 1. Wrap single values and arrays in a special marker: $_StartMultiVariable_..._EndMultiVariable
   * 2. Later, in replaceMultiVariables(), we convert these markers to proper LogsQL syntax:
   *    - For exact match: "app1" OR "app2" OR "app3"
   *    - For in() operator: in("app1","app2","app3")
   *    - For regex: (app1|app2|app3)
   * 
   * WHY THIS COMPLEXITY?
   * LogsQL has different syntax for different operators:
   * - field:"value" for single values
   * - field:in("v1","v2") for multi-value exact match
   * - field:~"(v1|v2)" for regex match
   * We need to defer the decision until we know the operator context.
   * 
   * @param value - The variable value (string or array)
   * @param _variable - The variable definition (unused but required by interface)
   * @returns Specially marked string for later conversion
   */
  interpolateQueryExpr(value: any, _variable: any) {
    // Convert single string to array for uniform handling
    if (typeof value === 'string' && value) {
      value = [value];
    }

    if (Array.isArray(value)) {
      // Mark multi-value variables for later conversion
      // The markers allow us to find and replace these in the final interpolation step
      return value.length > 0 ? `$_StartMultiVariable_${value.join('_separator_')}_EndMultiVariable` : '';
    }

    return value;
  }

  /**
   * interpolateVariablesInQueries - Batch Variable Interpolation
   * 
   * Interpolates variables in multiple queries at once.
   * This is used by Grafana for panel queries that need to be expanded
   * before being sent to the datasource.
   * 
   * @param queries - Array of queries to interpolate
   * @param scopedVars - Variables available in this context
   * @param filters - Ad-hoc filters to apply
   * @returns Array of queries with variables interpolated
   */
  interpolateVariablesInQueries(queries: Query[], scopedVars: ScopedVars, filters?: AdHocVariableFilter[]): Query[] {
    let expandedQueries = queries;
    if (queries && queries.length) {
      expandedQueries = queries.map((query) => ({
        ...query,
        datasource: this.getRef(),
        expr: this.interpolateString(query.expr, scopedVars),
        interval: this.templateSrv.replace(query.interval, scopedVars),
        extraFilters: this.getExtraFilters(filters, query.extraFilters),
      }));
    }
    return expandedQueries;
  }

  /**
   * metricFindQuery - Execute Template Variable Query
   * 
   * This method is called by Grafana when evaluating template variable queries.
   * Template variable queries allow users to populate dropdown menus dynamically
   * from VictoriaLogs data (e.g., list of all unique hostnames).
   * 
   * USE CASES:
   * - Variable of type "query" needs to fetch field names or values
   * - Ad-hoc filter dropdowns need lists of available fields
   * 
   * @param query - The variable query (type, field, filter expression)
   * @param options - Query options including time range and scoped variables
   * @returns Promise resolving to array of values for the dropdown
   */
  async metricFindQuery(
    query: VariableQuery,
    options?: LegacyMetricFindQueryOptions
  ): Promise<MetricFindValue[]> {
    if (!query) {
      return Promise.resolve([]);
    }

    // Interpolate any variables in the query parameters
    // For example, field: "$selected_field" → field: "hostname"
    const interpolatedVariableQuery: VariableQuery = {
      ...query,
      field: this.interpolateString(query.field || '', options?.scopedVars),
      query: this.interpolateString(query.query || '', options?.scopedVars),
    };

    return await this.processMetricFindQuery(interpolatedVariableQuery, options?.range);
  }

  /**
   * getTagKeys - Get Available Field Names for Ad-hoc Filters
   * 
   * Called by Grafana to populate the ad-hoc filter key dropdown.
   * Returns a list of all field names available in VictoriaLogs.
   * 
   * @param options - Options including time range for filtering
   * @returns Promise resolving to array of field names
   */
  async getTagKeys(options?: DataSourceGetTagKeysOptions<Query>): Promise<MetricFindValue[]> {
    const list = await this.languageProvider?.getFieldList({
      type: FilterFieldType.FieldName,
      timeRange: options?.timeRange,
      limit: DEFAULT_FIELD_DISPLAY_VALUES_LIMIT,
    }, this.customQueryParameters);
    return list
      ? list.map(({ value }) => ({ text: value || ' ' }))
      : [];
  }

  /**
   * getTagValues - Get Available Field Values for Ad-hoc Filters
   * 
   * Called by Grafana to populate the ad-hoc filter value dropdown.
   * Returns a list of unique values for a specific field.
   * 
   * @param options - Options including the field name and time range
   * @returns Promise resolving to array of field values
   */
  async getTagValues(options: DataSourceGetTagValuesOptions<Query>): Promise<MetricFindValue[]> {
    const list = await this.languageProvider?.getFieldList({
      type: FilterFieldType.FieldValue,
      timeRange: options.timeRange,
      limit: DEFAULT_FIELD_DISPLAY_VALUES_LIMIT,
      field: options.key,
    }, this.customQueryParameters);
    return list
      ? list.map(({ value }) => ({ text: value || ' ' }))
      : [];
  }

    if (Array.isArray(value)) {
      return value.length > 0 ? `$_StartMultiVariable_${value.join('_separator_')}_EndMultiVariable` : '';
    }

    return value;
  }

  interpolateVariablesInQueries(queries: Query[], scopedVars: ScopedVars, filters?: AdHocVariableFilter[]): Query[] {
    let expandedQueries = queries;
    if (queries && queries.length) {
      expandedQueries = queries.map((query) => ({
        ...query,
        datasource: this.getRef(),
        expr: this.interpolateString(query.expr, scopedVars),
        interval: this.templateSrv.replace(query.interval, scopedVars),
        extraFilters: this.getExtraFilters(filters, query.extraFilters),
      }));
    }
    return expandedQueries;
  }

  async metricFindQuery(query: VariableQuery, options?: LegacyMetricFindQueryOptions): Promise<MetricFindValue[]> {
    if (!query) {
      return Promise.resolve([]);
    }

    const interpolatedVariableQuery: VariableQuery = {
      ...query,
      field: this.interpolateString(query.field || '', options?.scopedVars),
      query: this.interpolateString(query.query || '', options?.scopedVars),
    };

    return await this.processMetricFindQuery(interpolatedVariableQuery, options?.range);
  }

  async getTagKeys(options?: DataSourceGetTagKeysOptions<Query>): Promise<MetricFindValue[]> {
    const list = await this.languageProvider?.getFieldList(
      {
        type: FilterFieldType.FieldName,
        timeRange: options?.timeRange,
        limit: DEFAULT_FIELD_DISPLAY_VALUES_LIMIT,
      },
      this.customQueryParameters
    );
    return list ? list.map(({ value }) => ({ text: value || ' ' })) : [];
  }

  async getTagValues(options: DataSourceGetTagValuesOptions<Query>): Promise<MetricFindValue[]> {
    const list = await this.languageProvider?.getFieldList(
      {
        type: FilterFieldType.FieldValue,
        timeRange: options.timeRange,
        limit: DEFAULT_FIELD_DISPLAY_VALUES_LIMIT,
        field: options.key,
      },
      this.customQueryParameters
    );
    return list ? list.map(({ value }) => ({ text: value || ' ' })) : [];
  }

  /**
   * isAllOption - Check if a Variable Has the "All" Option Selected
   * 
   * Grafana template variables can have a special "All" option that selects all values.
   * When "All" is selected, we need to handle the query differently (no filtering).
   * 
   * @param variable - The variable to check
   * @returns true if "All" is selected
   */
  isAllOption(variable: TypedVariableModel): boolean {
    const value = 'current' in variable && variable?.current?.value;
    if (!value) {
      return false;
    }

    if (typeof value === 'string') {
      return value === VARIABLE_ALL_VALUE || value === TEXT_FILTER_ALL_VALUE;
    }

    return Array.isArray(value) ? value.includes(VARIABLE_ALL_VALUE) : false;
  }

  /**
   * replaceOperatorsToInForMultiQueryVariables - Convert Multi-value Variables to "in()" Syntax
   * 
   * This is a critical transformation for multi-value template variables.
   * When a user selects multiple values in a dropdown (e.g., ["app1", "app2"]),
   * we need to convert the simple equality operator to the "in()" operator.
   * 
   * TRANSFORMATION EXAMPLES:
   * - app:$app → app:in("app1","app2")  (when $app = ["app1", "app2"])
   * - app:!$app → !app:in("app1","app2")  (negated version)
   * 
   * WHY THIS IS NEEDED:
   * LogsQL doesn't support array expansion like PromQL. We need to explicitly
   * use the "in()" operator for multi-value matching.
   * 
   * @param expr - The query expression with template variables
   * @returns Expression with multi-value variables converted to "in()" syntax
   */
  replaceOperatorsToInForMultiQueryVariables(expr: string) {
    const variables = this.templateSrv.getVariables();
    
    // Find all query-type variables that are multi-value or have "All" selected
    const fieldValuesVariables = variables.filter(v => 
      v.type === 'query' && 
      v.query.type === 'fieldValue' && 
      (v.multi || this.isAllOption(v))
    ) as QueryVariableModel[];
    
    let result = expr;
    
    // Process each multi-value variable
    for (const variable of fieldValuesVariables) {
      // Remove double quotes around the variable (added by Grafana)
      result = removeDoubleQuotesAroundVar(result, variable.name);
      
      // Replace = or != operators with in() or not_in()
      result = replaceOperatorWithIn(result, variable.name);
    }
    
    return result;
  }

  /**
   * interpolateString - Full Variable Interpolation Pipeline
   * 
   * This is the main entry point for variable interpolation. It runs through
   * a series of transformations to properly handle all variable types.
   * 
   * PIPELINE STEPS:
   * 1. Convert multi-value variables to "in()" syntax
   * 2. Handle regex operators in variable values
   * 3. Perform standard Grafana variable replacement
   * 4. Correct regex values for "All" option
   * 5. Correct multi-exact operator values for "All" option
   * 6. Replace multi-variable markers with final syntax
   * 
   * WHY MULTIPLE STEPS?
   * Each step handles a specific edge case:
   * - Multi-value variables need special syntax
   * - Regex values need escaping
   * - "All" option needs special handling
   * - Different operators need different final formats
   * 
   * @param string - The string with template variables
   * @param scopedVars - Variables available in this context
   * @returns String with all variables interpolated
   */
  interpolateString(string: string, scopedVars?: ScopedVars) {
    // Step 1: Convert multi-value query variables to "in()" syntax
    let expr = this.replaceOperatorsToInForMultiQueryVariables(string);
    
    // Step 2: Get list of variable names for regex handling
    const variableNamesList = this.templateSrv.getVariables().map(v => v.name);
    
    // Step 3: Double-quote regex patterns in variable values
    // This ensures regex patterns are properly escaped in LogsQL
    expr = doubleQuoteRegExp(expr, variableNamesList);
    
    // Step 4: Perform standard Grafana variable replacement
    // Uses our custom interpolateQueryExpr for multi-value handling
    expr = this.templateSrv.replace(expr, scopedVars, this.interpolateQueryExpr);
    
    // Step 5: Correct regex values when "All" option is selected
    expr = correctRegExpValueAll(expr);
    
    // Step 6: Correct multi-exact operator values when "All" is selected
    expr = correctMultiExactOperatorValueAll(expr);
    
    // Step 7: Convert multi-variable markers to final LogsQL syntax
    return this.replaceMultiVariables(expr);
  }

  /**
   * replaceMultiVariables - Convert Variable Markers to LogsQL Syntax
   * 
   * This private method converts the special markers we added in interpolateQueryExpr
   * into the final LogsQL syntax based on the surrounding context.
   * 
   * MARKER FORMAT: $_StartMultiVariable_value1_separator_value2_EndMultiVariable
   * 
   * CONVERSION RULES:
   * 1. After =~ (regex operator): Convert to (v1|v2|v3)
   * 2. After in( (in operator): Convert to "v1","v2","v3"
   * 3. Default: Convert to "v1" OR "v2" OR "v3"
   * 
   * WHY CONTEXT MATTERS:
   * Different operators require different syntax:
   * - Regex: field:~"(v1|v2|v3)"
   * - In: field:in("v1","v2","v3")
   * - OR: field:"v1" OR field:"v2" OR field:"v3"
   * 
   * @param input - String with variable markers
   * @returns String with markers replaced by LogsQL syntax
   */
  private replaceMultiVariables(input: string): string {
    // Pattern to find our special multi-variable markers
    const multiVariablePattern = /\$_StartMultiVariable_(.+?)_EndMultiVariable?/g;

    return input.replace(multiVariablePattern, (match, valueList: string, offset) => {
      // Split the values by our separator
      const values = valueList.split('_separator_');

      // Look at the context before this variable to determine the operator
      const queryBeforeOffset = input.slice(0, offset);
      const precedingChars = queryBeforeOffset.replace(/\s+/g, '').slice(-3);

      if (isRegExpOperatorInLastFilter(queryBeforeOffset)) {
        // Regex operator: convert to (v1|v2|v3) format
        return `(${values.join('|')})`;
      } else if (precedingChars.includes('in(')) {
        // In operator: convert to "v1","v2","v3" format
        return values.map(value => JSON.stringify(value)).join(',');
      }
      
      // Default: OR syntax for multiple filters
      return values.join(' OR ');
    });
  }

    if (typeof value === 'string') {
      return value === VARIABLE_ALL_VALUE || value === TEXT_FILTER_ALL_VALUE;
    }

    return Array.isArray(value) ? value.includes(VARIABLE_ALL_VALUE) : false;
  }

  replaceOperatorsToInForMultiQueryVariables(expr: string) {
    const variables = this.templateSrv.getVariables();
    const fieldValuesVariables = variables.filter(
      (v) => (v.type === 'query' && v.query.type === 'fieldValue' && v.multi) || this.isAllOption(v)
    ) as QueryVariableModel[];
    let result = expr;
    for (const variable of fieldValuesVariables) {
      result = removeDoubleQuotesAroundVar(result, variable.name);
      result = replaceOperatorWithIn(result, variable.name);
    }
    return result;
  }

  interpolateString(string: string, scopedVars?: ScopedVars) {
    let expr = this.replaceOperatorsToInForMultiQueryVariables(string);
    const variableNamesList = this.templateSrv.getVariables().map((v) => v.name);
    expr = doubleQuoteRegExp(expr, variableNamesList);
    expr = this.templateSrv.replace(expr, scopedVars, this.interpolateQueryExpr);
    expr = correctRegExpValueAll(expr);
    expr = correctMultiExactOperatorValueAll(expr);
    return this.replaceMultiVariables(expr);
  }

  private replaceMultiVariables(input: string): string {
    const multiVariablePattern = /\$_StartMultiVariable_(.+?)_EndMultiVariable?/g;

    return input.replace(multiVariablePattern, (match, valueList: string, offset) => {
      const values = valueList.split('_separator_');

      const queryBeforeOffset = input.slice(0, offset);
      const precedingChars = queryBeforeOffset.replace(/\s+/g, '').slice(-3);

      if (isRegExpOperatorInLastFilter(queryBeforeOffset)) {
        return `(${values.join('|')})`;
      } else if (precedingChars.includes('in(')) {
        return values.map((value) => JSON.stringify(value)).join(',');
      }
      return values.join(' OR ');
    });
  }

  /**
   * processMetricFindQuery - Execute a Variable Query via Language Provider
   * 
   * Private helper that executes variable queries through the language provider.
   * The language provider caches results and communicates with the backend
   * to fetch field names and values.
   * 
   * @param query - The variable query (type, field, filter, limit)
   * @param timeRange - Time range for filtering results
   * @returns Promise resolving to array of values
   */
  private async processMetricFindQuery(query: VariableQuery, timeRange?: TimeRange): Promise<MetricFindValue[]> {
    const list = await this.languageProvider?.getFieldList({
      type: query.type,
      timeRange,
      field: query.field,
      query: query.query,
      limit: query.limit,
    }, this.customQueryParameters);
    return (list ? list.map(({ value }) => ({ text: value })) : []);
  }

  /**
   * getQueryBuilderLimits - Get Autocomplete Limits for Query Builder
   * 
   * Returns the configured limit for autocomplete results in the query builder.
   * This prevents the UI from being overwhelmed with thousands of field values.
   * 
   * @param key - The type of limit (field names, field values, etc.)
   * @returns The configured limit or 0 (unlimited)
   */
  getQueryBuilderLimits(key: FilterFieldType): number {
    return this.queryBuilderLimits?.[key] || 0;
  }

  /**
   * runLiveQueryThroughBackend - Execute Live Streaming Query
   * 
   * Implements live log tailing using Grafana's Live feature.
   * This creates a WebSocket-like connection that streams new logs in real-time.
   * 
   * HOW IT WORKS:
   * 1. Frontend creates a Live channel for each query
   * 2. Backend (Go) opens a streaming connection to VictoriaLogs /tail endpoint
   * 3. New logs are streamed from VictoriaLogs → Go backend → Grafana Live → Frontend
   * 4. Multiple clients can subscribe to the same channel
   * 
   * WHY GRAFANA LIVE?
   * We can't use direct WebSocket from browser because:
   * - VictoriaLogs credentials must stay on the server
   * - We need to handle authentication and multi-tenancy
   * - Grafana Live provides connection management and scaling
   * 
   * @param request - The query request with liveStreaming=true
   * @returns Observable that emits logs as they arrive
   */
  private runLiveQueryThroughBackend(request: DataQueryRequest<Query>): Observable<DataQueryResponse> {
    const observables = request.targets.map((query) => {
      return getGrafanaLiveSrv()
        .getDataStream({
          addr: {
            scope: LiveChannelScope.DataSource,
            namespace: this.uid,
            // @ts-expect-error - from the Grafana with React 19 version,
            // the interface of the Live feature expects the `stream` field instead of the `namespace`,
            // so we need to send both for compatibility with older versions
            stream: this.uid,
            // Path format: requestId/refId - uniquely identifies this query stream
            path: `${request.requestId}/${query.refId}`,
            data: {
              ...query,
            },
          },
        })
        .pipe(
          map((response) => {
            return {
              data: response.data || [],
              key: `victoriametrics-logs-datasource-${request.requestId}-${query.refId}`,
              state: LoadingState.Streaming,
            };
          })
        );
    });

    // Merge all query streams into a single observable
    return merge(...observables);
  }

  getQueryBuilderLimits(key: FilterFieldType): number {
    return this.queryBuilderLimits?.[key] || 0;
  }

  private runLiveQueryThroughBackend(request: DataQueryRequest<Query>): Observable<DataQueryResponse> {
    const observables = request.targets.map((query) => {
      return getGrafanaLiveSrv()
        .getDataStream({
          addr: {
            scope: LiveChannelScope.DataSource,
            namespace: this.uid,
            // @ts-expect-error - from the Grafana with React 19 version,
            // the interface of the Live feature expects the `stream` field instead of the `namespace`,
            // so we need to send both for compatibility with older versions
            stream: this.uid,
            path: `${request.requestId}/${query.refId}`,
            data: {
              ...query,
            },
          },
        })
        .pipe(
          map((response) => {
            return {
              data: response.data || [],
              key: `victoriametrics-logs-datasource-${request.requestId}-${query.refId}`,
              state: LoadingState.Streaming,
            };
          })
        );
    });

    return merge(...observables);
  }

  getSupplementaryRequest(
    type: SupplementaryQueryType,
    request: DataQueryRequest<Query>,
    options?: SupplementaryQueryOptions
  ): DataQueryRequest<Query> | undefined {
    const logsVolumeOption = { ...options, type };
    const logsVolumeRequest = cloneDeep(request);
    const targets = logsVolumeRequest.targets
      .map((query) => this.getSupplementaryQuery(logsVolumeOption, query, logsVolumeRequest))
      .filter((query): query is Query => !!query);

    if (!targets.length) {
      return undefined;
    }

    return { ...logsVolumeRequest, targets };
  }

  getSupportedSupplementaryQueryTypes(): SupplementaryQueryType[] {
    return [SupplementaryQueryType.LogsVolume, SupplementaryQueryType.LogsSample];
  }

  getSupplementaryQuery(
    options: SupplementaryQueryOptions,
    query: Query,
    request: DataQueryRequest<Query>
  ): Query | undefined {
    if (query.hide) {
      return undefined;
    }

    switch (options.type) {
      case SupplementaryQueryType.LogsVolume: {
        const totalSeconds = request.range.to.diff(request.range.from, 'second');
        const step = Math.ceil(totalSeconds / LOGS_VOLUME_BARS) || '';

        const fields = this.getActiveLevelRules().map((r) => r.field);
        const uniqFields = Array.from(new Set([...fields, 'level']));

        return {
          ...query,
          step: `${step}s`,
          fields: uniqFields,
          queryType: QueryType.Hits,
          refId: `${REF_ID_STARTER_LOG_VOLUME}${query.refId}`,
          supportingQueryType: SupportingQueryType.LogsVolume,
          timezoneOffset: formatOffsetDuration(request.timezone, request.range.from.utcOffset()),
        };
      }
      case SupplementaryQueryType.LogsSample:
        return {
          ...query,
          queryType: QueryType.Instant,
          refId: `${REF_ID_STARTER_LOG_SAMPLE}${query.refId}`,
          supportingQueryType: SupportingQueryType.LogsSample,
          maxLines: this.maxLines,
        };

      default:
        return undefined;
    }
  }

  getDataProvider(
    type: SupplementaryQueryType,
    request: DataQueryRequest<Query>
  ): Observable<DataQueryResponse> | undefined {
    if (!this.getSupportedSupplementaryQueryTypes().includes(type)) {
      return undefined;
    }

    const newRequest = this.getSupplementaryRequest(type, request);
    if (!newRequest) {
      return;
    }

    switch (type) {
      case SupplementaryQueryType.LogsVolume:
        return queryLogsVolume(this, newRequest);
      default:
        return undefined;
    }
  }

  getQueryDisplayText(query: Query): string {
    return query.expr || '';
  }

  getActiveLevelRules(): LogLevelRule[] {
    return (this.logLevelRules || []).filter((r) => r.enabled !== false);
  }

  getLogRowContext = async (row: LogRowModel, options?: LogRowContextOptions): Promise<{ data: DataFrame[] }> => {
    const contextRequest = this.makeLogContextDataRequest(row, options);
    return lastValueFrom(this.runQuery(contextRequest));
  };

  private prepareLogContextQueryExpr = (row: LogRowModel): string => {
    let streamId = '';
    const streamIds = row.dataFrame.meta?.custom?.streamIds;
    if (streamIds && streamIds.length > 0) {
      streamId = streamIds[row.rowIndex];
    }

    if (!streamId && row.labels[LABEL_STREAM_ID]) {
      // Explore View
      streamId = row.labels[LABEL_STREAM_ID];
    } else if (!streamId) {
      // Dashboard View
      const transformedLabels: Labels = {};
      Object.values(row.labels).forEach((label) => {
        const [key, value] = label.split(':');
        const cleanedKey = key.trim();
        transformedLabels[cleanedKey] = value.trim().replace(/"/g, '');
      });
      streamId = transformedLabels[LABEL_STREAM_ID];
    }

    return addLabelToQuery('', { key: LABEL_STREAM_ID, value: streamId, operator: '' });
  };

  private makeLogContextDataRequest = (row: LogRowModel, options?: LogRowContextOptions): DataQueryRequest<Query> => {
    const direction = options?.direction || LogRowContextQueryDirection.Backward;

    const query: Query = {
      expr: this.prepareLogContextQueryExpr(row),
      refId: `${REF_ID_STARTER_LOG_CONTEXT_QUERY}${row.dataFrame.refId}-${options?.direction}`,
    };

    const range = this.createContextTimeRange(row.timeEpochMs, direction);

    const interval = rangeUtil.calculateInterval(range, 1);

    return {
      app: CoreApp.Explore,
      interval: interval.interval,
      intervalMs: interval.intervalMs,
      range: range,
      requestId: `${REF_ID_STARTER_LOG_CONTEXT_REQUEST}${row.dataFrame.refId}-${options?.direction}`,
      scopedVars: {},
      startTime: Date.now(),
      targets: [query],
      timezone: 'UTC',
    };
  };

  private createContextTimeRange = (rowTimeEpochMs: number, direction?: LogRowContextQueryDirection): TimeRange => {
    const offset = 2 * 60 * 60 * 1000; // 2h
    const overlap = 1000;

    const timeRange =
      direction === LogRowContextQueryDirection.Backward
        ? {
            from: toUtc(rowTimeEpochMs - offset),
            to: toUtc(rowTimeEpochMs + overlap),
          }
        : {
            from: toUtc(rowTimeEpochMs),
            to: toUtc(rowTimeEpochMs + offset), // Add 1 second to avoid missing results
          };

    return { ...timeRange, raw: timeRange };
  };

  async fetchTenantIds(): Promise<{ hint: string } | string[]> {
    try {
      const res = await this.postResource<{ hint: string } | Tenant[]>('select/tenant_ids', {});

      if (!Array.isArray(res)) {
        if (res.hint) {
          return res;
        }
        return [];
      }

      const tenantSet = new Set<string>();
      res.forEach((item: Tenant) => {
        tenantSet.add(`${item.account_id}:${item.project_id}`);
      });

      return Array.from(tenantSet);
    } catch (error) {
      console.error('Failed to fetch tenants:', error);
      return [];
    }
  }

  parseMultitenancyHeaders(multitenancyHeaders?: Partial<Record<TenantHeaderNames, string>>): MultitenancyHeaders {
    const formatTenantId = (value: string | number | undefined): string => {
      if (value === undefined || value === '') {
        return '0';
      }
      const num = Number(value);
      return Number.isInteger(num) ? String(num) : '0';
    };

    return {
      [TenantHeaderNames.AccountID]: formatTenantId(multitenancyHeaders?.AccountID),
      [TenantHeaderNames.ProjectID]: formatTenantId(multitenancyHeaders?.ProjectID),
    };
  }
}
