/**
 * Response Transformation Pipeline - Enriching Backend Responses
 * 
 * This is the critical response transformation function that processes raw data frames
 * from the backend and enriches them with additional metadata before
 * Grafana renders them in the UI.
 * 
 * ARCHITECTURE:
 * The function receives raw data frames from the backend (Go) and:
 * 1. Validates that all data items are data frames
 * 2. Groups frames by their intended use case (logs, metrics, histograms)
 * 3. Processes each group with appropriate transformations:
 *    - Streams (logs): Add derived fields, log levels, labels
 *    - Metrics (instant/range): Format for time series visualization
 *    - Histograms: Format for histogram visualization
 * 4. Impro error messages to be more user-friendly
 * 5. Returns enriched data to Grafana
 * 
 * WHY THIS TRANSFORMATION?
 * The backend returns minimal data frames for efficiency:
 * - Raw JSON/NDJSON parsing is done in backend (Go)
 * - Minimal transformation in backend reduces latency
 * - Frontend adds context-dependent features:
 *   - Derived fields: User-configured patterns to extract links from logs
 *   - Log levels: Custom rules for detecting log severity
 *   - Label formatting: Makes logs easier to read in the UI
 * 
 * WHEN THIS RUNS:
 * - After every query completes (both regular and live queries)
 * - Called by datasource.ts:runQuery() via RxJS map operator
 * 
 * TRANSFORMATION TYPES:
 * 1. Streams (logs):
 *    - Input: Raw log lines with _msg, _stream, _time fields
 *    - Output: Enriched logs with derived fields, log levels, formatted labels
 * 
 * 2. Metric Instant:
 *    - Input: Point-in-time metric values
 *    - Output: Time series data formatted for charts
 * 
 * 3. Metric Range:
 *    - Input: Time series data over a range
 *    - Output: Time series data with proper interval metadata
 * 
 * 4. Histogram:
 *    - Input: Bucketed log counts
 *    - Output: Histogram visualization data
 * 
 * For more details, see:
 * - onboarding/onboarding-system-overview.md
 * - onboarding/onboarding-query-flow.md
 */

import { DataQueryError, DataQueryRequest, DataQueryResponse, isDataFrame } from '@grafana/data';

import { LogLevelRule } from '../configuration/LogLevelRules/types';
import { DerivedFieldConfig, Query } from '../types';

import {
  processHistogramFrames,
  processMetricInstantFrames
  processMetricRangeFrames
  processStreamsFrames
} from './frameProcessors';
import { improveError } from './utils/errorUtils';
import { getQueryMap, groupFrames } from './utils/frame/frameUtils';

/**
 * transformBackendResult - Main Response Transformation Function
 * 
 * This function orchestrates the entire response transformation pipeline.
 * It's called by datasource.ts after receiving a response from the backend.
 * 
 * @param response - Raw response from the backend (Go)
 * @param request - Original query request (contains query details)
 * @param derivedFieldConfigs - User-configured derived field patterns
 * @param logLevelRules - User-configured log level detection rules
 * @returns Enriched response ready for Grafana to render
 */
export function transformBackendResult(
  response: DataQueryResponse,
  request: DataQueryRequest<Query>,
  derivedFieldConfigs: DerivedFieldConfig[],
  logLevelRules: LogLevelRule[],
): DataQueryResponse {
  const { data, errors, ...rest } = response;
  const queries = request.targets;

  // Validate that all data items are data frames
  // Grafana's type system allows any data, but we know it backend only returns data frames
  const dataFrames = data.map((d) => {
    if (!isDataFrame(d)) {
      throw new Error('transformation only supports dataframe responses');
    }

    return d;
  });

  // Build a map of query RefIDs to query objects for looking up query details
  const queryMap = getQueryMap(queries) as Map<string, Query>;

  // Group frames by their type (streams, metrics, histograms)
  // This allows us to apply different transformations to each group
  const { streamsFrames, metricInstantFrames, metricRangeFrames, histogramFrames } = groupFrames(dataFrames, queryMap);

  // Improve error messages to be more user-friendly
  // VictoriaLogs errors can be technical, so we make them more actionable
  const improvedErrors = errors && errors.map((error) => improveError(error, queryMap)).filter((e) => e !== undefined);

  // Process each group and appropriate transformations and return the enriched response
  return {
    ...rest,
    errors: improvedErrors as DataQueryError[],
    data: [
      // Metric range frames: Time series data with interval metadata
      ...processMetricRangeFrames(metricRangeFrames, request.targets, request.range.from.valueOf(), request.range.to.valueOf()),
      // Metric instant frames: Point-in-time metric values
      ...processMetricInstantFrames(metricInstantFrames),
      // Stream frames: Logs with derived fields, log levels, labels
      ...processStreamsFrames(streamsFrames, queryMap, derivedFieldConfigs, logLevelRules),
      // Histogram frames: Bucketed log counts
      ...processHistogramFrames(histogramFrames),
    ],
  };
}

    return d;
  });

  const queryMap = getQueryMap(queries) as Map<string, Query>;

  const { streamsFrames, metricInstantFrames, metricRangeFrames, histogramFrames } = groupFrames(dataFrames, queryMap);

  const improvedErrors = errors && errors.map((error) => improveError(error, queryMap)).filter((e) => e !== undefined);

  return {
    ...rest,
    errors: improvedErrors as DataQueryError[],
    data: [
      ...processMetricRangeFrames(metricRangeFrames, request.targets, request.range.from.valueOf(), request.range.to.valueOf()),
      ...processMetricInstantFrames(metricInstantFrames),
      ...processStreamsFrames(streamsFrames, queryMap, derivedFieldConfigs, logLevelRules),
      ...processHistogramFrames(histogramFrames),
    ],
  };
}
