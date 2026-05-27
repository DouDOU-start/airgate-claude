import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageRecordLike {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cached_input_tokens?: number;
  cache_creation_tokens?: number;
  reasoning_output_tokens?: number;
  usage_metadata?: Record<string, string>;
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

const inlineValueStyle: CSSProperties = {
  display: 'inline-flex',
  minWidth: 0,
  maxWidth: '100%',
  alignItems: 'baseline',
  justifyContent: 'flex-end',
  gap: '0.25rem',
};

const inlineValueMetaStyle: CSSProperties = {
  minWidth: 0,
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const inlineValueNumberStyle: CSSProperties = {
  flexShrink: 0,
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

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function metadataFromContext(context: UsageRecordSurfaceProps['context'], record: UsageRecordLike): Record<string, string> {
  const direct = context?.usage_metadata;
  if (direct && typeof direct === 'object') return direct as Record<string, string>;
  return record.usage_metadata ?? {};
}

function metadataNumber(metadata: Record<string, string>, key: string) {
  const value = Number(metadata[key]);
  return Number.isFinite(value) ? value : 0;
}

function formatNumber(value: number) {
  return Number.isInteger(value)
    ? value.toLocaleString()
    : value.toLocaleString(undefined, { maximumFractionDigits: 4 });
}

function Row({ label, tone, value }: { label: ReactNode; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

function outputTokenValue(reasoningTokens: number, outputTokens: number) {
  return (
    <span style={inlineValueStyle}>
      {reasoningTokens > 0 ? (
        <span style={inlineValueMetaStyle}>(推理 {formatNumber(reasoningTokens)})</span>
      ) : null}
      <span style={inlineValueNumberStyle}>{formatNumber(outputTokens)}</span>
    </span>
  );
}

export function UsageMetricDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const metadata = metadataFromContext(context, record);
  const inputTokens = record.input_tokens || 0;
  const outputTokens = record.output_tokens || 0;
  const cacheReadTokens = record.cached_input_tokens || 0;
  const cacheCreationTokens = record.cache_creation_tokens || 0;
  const cacheCreation5mTokens = metadataNumber(metadata, 'claude.cache_creation_5m_tokens');
  const cacheCreation1hTokens = metadataNumber(metadata, 'claude.cache_creation_1h_tokens');
  const reasoningTokens = record.reasoning_output_tokens || 0;
  const totalTokens = inputTokens + outputTokens + cacheReadTokens + cacheCreationTokens;

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>Claude 计量明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      <div style={bodyStyle}>
        <Row label="输入 Token" value={formatNumber(inputTokens)} tone="var(--ag-info)" />
        <Row label="输出 Token" value={outputTokenValue(reasoningTokens, outputTokens)} tone="var(--ag-primary)" />
        {cacheReadTokens > 0 ? <Row label="缓存读取 Token" value={formatNumber(cacheReadTokens)} tone="var(--ag-success)" /> : null}
        {cacheCreationTokens > 0 ? <Row label="缓存写入 Token" value={formatNumber(cacheCreationTokens)} tone="var(--ag-warning)" /> : null}
        {cacheCreation5mTokens > 0 ? <Row label="缓存写入 5m" value={formatNumber(cacheCreation5mTokens)} tone="var(--ag-warning)" /> : null}
        {cacheCreation1hTokens > 0 ? <Row label="缓存写入 1h" value={formatNumber(cacheCreation1hTokens)} tone="var(--ag-warning)" /> : null}
        <Row label="总 Token" value={formatNumber(totalTokens)} tone="var(--ag-text)" />
      </div>
    </div>
  );
}
