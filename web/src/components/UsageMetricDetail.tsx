import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageMetric {
  key?: string;
  label?: string;
  kind?: string;
  unit?: string;
  value?: number;
}

interface UsageRecordLike {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cached_input_tokens?: number;
  cache_creation_tokens?: number;
  cache_creation_5m_tokens?: number;
  cache_creation_1h_tokens?: number;
  reasoning_output_tokens?: number;
}

const panelStyle: CSSProperties = {
  overflow: 'hidden',
  borderRadius: 'var(--radius)',
};

const headerStyle: CSSProperties = {
  borderBottom: '1px solid var(--ag-border)',
  background: 'var(--ag-default-bg)',
  padding: '0.375rem 0.625rem',
};

const titleStyle: CSSProperties = {
  color: 'var(--ag-text)',
  fontSize: '0.875rem',
  fontWeight: 600,
  lineHeight: 1,
};

const subtitleStyle: CSSProperties = {
  marginTop: '0.25rem',
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  fontSize: '0.75rem',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const bodyStyle: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.125rem',
  padding: '0.5rem',
};

const rowStyle: CSSProperties = {
  display: 'grid',
  gridTemplateColumns: 'minmax(0,1fr) minmax(7rem,max-content)',
  alignItems: 'center',
  gap: '0.75rem',
  borderRadius: 'var(--radius)',
  background: 'var(--ag-surface)',
  padding: '0.25rem 0.5rem',
  fontSize: '0.75rem',
};

const labelStyle: CSSProperties = {
  minWidth: 0,
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const valueStyle: CSSProperties = {
  minWidth: 0,
  maxWidth: '12rem',
  justifySelf: 'end',
  overflow: 'hidden',
  color: 'var(--ag-text-secondary)',
  fontFamily: 'var(--ag-font-mono)',
  fontWeight: 500,
  textAlign: 'right',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const subLabelStyle: CSSProperties = {
  ...labelStyle,
  paddingLeft: '0.75rem',
  color: 'var(--ag-text-quaternary, var(--ag-text-tertiary))',
};

function contextArray<T>(context: UsageRecordSurfaceProps['context'], camel: string, snake: string): T[] {
  const value = context?.[camel] ?? context?.[snake];
  return Array.isArray(value) ? value as T[] : [];
}

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function norm(value?: string) {
  return (value || '').trim().toLowerCase().replace(/[\s-]+/g, '_');
}

function numberValue(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function formatNumber(value: number) {
  return Number.isInteger(value)
    ? value.toLocaleString()
    : value.toLocaleString(undefined, { maximumFractionDigits: 4 });
}

function metricValue(metrics: UsageMetric[], keys: string[]) {
  const metric = metrics.find((item) => keys.includes(norm(item.key || item.kind || item.label)));
  return metric ? numberValue(metric.value) : 0;
}

function Row({ label, sub, tone, value }: { label: ReactNode; sub?: boolean; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={sub ? subLabelStyle : labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

function cacheCreationValue(total: number, fiveMinute: number, oneHour: number): ReactNode {
  if (fiveMinute > 0 && oneHour === 0 && fiveMinute === total) {
    return `${formatNumber(total)}（5m）`;
  }
  if (oneHour > 0 && fiveMinute === 0 && oneHour === total) {
    return `${formatNumber(total)}（1h）`;
  }
  return formatNumber(total);
}

export function UsageMetricDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const metrics = contextArray<UsageMetric>(context, 'usageMetrics', 'usage_metrics');
  const inputTokens = metricValue(metrics, ['input_tokens', 'input_token', 'prompt_tokens', 'prompt_token']) || record.input_tokens || 0;
  const outputTokens = metricValue(metrics, ['output_tokens', 'output_token', 'completion_tokens', 'completion_token']) || record.output_tokens || 0;
  const cacheReadTokens = metricValue(metrics, ['cached_input_tokens', 'cached_input_token', 'cache_read_tokens', 'cache_read_token']) || record.cached_input_tokens || 0;
  const cacheCreationTokens = metricValue(metrics, ['cache_creation_tokens', 'cache_creation_input_tokens', 'cache_creation_token']) || record.cache_creation_tokens || 0;
  const cacheCreation5mTokens = metricValue(metrics, ['cache_creation_5m_tokens', 'cache_creation_5m_input_tokens']) || record.cache_creation_5m_tokens || 0;
  const cacheCreation1hTokens = metricValue(metrics, ['cache_creation_1h_tokens', 'cache_creation_1h_input_tokens']) || record.cache_creation_1h_tokens || 0;
  const cacheCreationTotal = cacheCreationTokens || cacheCreation5mTokens + cacheCreation1hTokens;
  const showCacheCreationBreakdown = cacheCreation5mTokens > 0 && cacheCreation1hTokens > 0;
  const reasoningTokens = metricValue(metrics, ['reasoning_output_tokens', 'reasoning_tokens', 'reasoning_token']) || record.reasoning_output_tokens || 0;
  const totalTokens = metricValue(metrics, ['total_tokens', 'total_token']) || inputTokens + outputTokens + cacheReadTokens + cacheCreationTotal;

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>Claude 计量明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      <div style={bodyStyle}>
        <Row label="输入 Token" value={formatNumber(inputTokens)} tone="var(--ag-info)" />
        <Row label="输出 Token" value={formatNumber(outputTokens)} tone="var(--ag-primary)" />
        {cacheReadTokens > 0 ? <Row label="缓存读取 Token" value={formatNumber(cacheReadTokens)} tone="var(--ag-success)" /> : null}
        {cacheCreationTotal > 0 ? (
          <Row
            label="缓存写入 Token"
            value={cacheCreationValue(cacheCreationTotal, cacheCreation5mTokens, cacheCreation1hTokens)}
            tone="var(--ag-warning)"
          />
        ) : null}
        {showCacheCreationBreakdown ? <Row sub label="其中 5m TTL" value={formatNumber(cacheCreation5mTokens)} tone="var(--ag-warning)" /> : null}
        {showCacheCreationBreakdown ? <Row sub label="其中 1h TTL" value={formatNumber(cacheCreation1hTokens)} tone="var(--ag-warning)" /> : null}
        {reasoningTokens > 0 ? <Row label="推理 Token" value={formatNumber(reasoningTokens)} tone="var(--ag-warning)" /> : null}
        <Row label="总 Token" value={formatNumber(totalTokens)} tone="var(--ag-text)" />
      </div>
    </div>
  );
}
