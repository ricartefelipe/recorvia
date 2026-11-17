import { Badge, Code, ScrollArea, Text, type BadgeProps } from '@mantine/core';
import { useMemo } from 'react';
import { formatCivilDate, formatDateTime, formatInstantDate } from '../lib/dates';
import { labelOf, statusColor } from '../lib/labels';
import { formatMoney, type MinorUnits } from '../lib/money';
import { useTenant } from '../session/SessionContext';

/** Formatadores no fuso da organização ativa. */
export function useFormat() {
  const { timeZone } = useTenant();
  return useMemo(
    () => ({
      timeZone,
      money: (value: MinorUnits | null | undefined, currency?: string) => formatMoney(value, currency),
      dateTime: (value: string | null | undefined) => formatDateTime(value, timeZone),
      date: (value: string | null | undefined) => formatInstantDate(value, timeZone),
      civil: (value: string | null | undefined) => formatCivilDate(value),
    }),
    [timeZone],
  );
}

export function Money({ value, currency, strong }: { value: MinorUnits | null | undefined; currency?: string; strong?: boolean }) {
  return (
    <Text span inherit fw={strong ? 600 : undefined} style={{ fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}>
      {formatMoney(value, currency)}
    </Text>
  );
}

export function DateTime({ value, dateOnly }: { value: string | null | undefined; dateOnly?: boolean }) {
  const f = useFormat();
  if (!value) return <>—</>;
  return (
    <time dateTime={value} title={`${f.dateTime(value)} (${f.timeZone})`}>
      {dateOnly ? f.date(value) : f.dateTime(value)}
    </time>
  );
}

export function CivilDate({ value }: { value: string | null | undefined }) {
  if (!value) return <>—</>;
  return <time dateTime={value}>{formatCivilDate(value)}</time>;
}

export function StatusBadge({
  status,
  labels,
  ...props
}: { status: string | null | undefined; labels: Record<string, string> } & Omit<BadgeProps, 'children' | 'color'>) {
  return (
    <Badge variant="light" color={statusColor(status)} {...props}>
      {labelOf(labels, status)}
    </Badge>
  );
}

export function JsonView({ value, maxHeight = 320 }: { value: unknown; maxHeight?: number }) {
  const text = useMemo(() => JSON.stringify(value ?? {}, null, 2), [value]);
  return (
    <ScrollArea.Autosize mah={maxHeight} type="auto">
      <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
        {text}
      </Code>
    </ScrollArea.Autosize>
  );
}

export function ShortId({ id }: { id: string | null | undefined }) {
  if (!id) return <>—</>;
  return (
    <Code title={id} style={{ whiteSpace: 'nowrap' }}>
      {id.slice(0, 8)}
    </Code>
  );
}
