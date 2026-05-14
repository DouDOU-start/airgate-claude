import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';
import type { CSSProperties } from 'react';

type UsageContext = {
  reasoning_effort?: string;
};

const EFFORT_COLORS: Record<string, string> = {
  low: 'rgb(34,197,94)',
  medium: 'rgb(59,130,246)',
  high: 'rgb(249,115,22)',
  xhigh: 'rgb(239,68,68)',
};

function chipStyle(color: string): CSSProperties {
  return {
    background: `color-mix(in srgb, ${color} 18%, transparent)`,
    boxShadow: `inset 0 0 0 1px color-mix(in srgb, ${color} 34%, transparent)`,
    color,
  };
}

export function UsageModelMeta(props: UsageRecordSurfaceProps) {
  const ctx = (props.context ?? {}) as UsageContext;
  if (!ctx.reasoning_effort) return null;

  const color = EFFORT_COLORS[ctx.reasoning_effort] ?? 'rgb(148,163,184)';

  return (
    <span
      className="inline-flex shrink-0 items-center rounded px-1.5 text-[12px] font-semibold leading-4 whitespace-nowrap"
      style={chipStyle(color)}
    >
      {ctx.reasoning_effort}
    </span>
  );
}
