"use client";

import { useId, useState } from "react";
import { fmtCount } from "@/lib/format";
import type { Count } from "@/lib/types";

export interface DayValue {
  day: string; // YYYY-MM-DD
  sessions: Count;
}

const MAX_DAYS = 366;

/**
 * Fills the days between from and to (inclusive) that the server omitted.
 * The server lists only days with sessions, so a missing day is a true zero
 * (zero is never suppressed). Suppressed days (null) are kept as null.
 */
export function fillDays(rows: DayValue[], from: string, to: string): DayValue[] {
  const byDay = new Map(rows.map((r) => [r.day, r.sessions]));
  const start = Date.parse(`${from}T00:00:00Z`);
  const end = Date.parse(`${to}T00:00:00Z`);
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return rows.slice(0, MAX_DAYS);
  const out: DayValue[] = [];
  for (let t = start; t <= end && out.length < MAX_DAYS; t += 86_400_000) {
    const day = new Date(t).toISOString().slice(0, 10);
    out.push({ day, sessions: byDay.has(day) ? (byDay.get(day) ?? null) : 0 });
  }
  return out;
}

/** A "nice" axis maximum (1, 2, 5 × 10^n) at or above v. */
export function niceMax(v: number): number {
  if (v <= 0) return 5;
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

const W = 720;
const H = 240;
const PAD = { top: 16, right: 12, bottom: 32, left: 44 };

function shortDay(d: string): string {
  const [, m, day] = d.split("-");
  const months = [
    "Jan",
    "Feb",
    "Mar",
    "Apr",
    "May",
    "Jun",
    "Jul",
    "Aug",
    "Sep",
    "Oct",
    "Nov",
    "Dec",
  ];
  return `${Number(day)} ${months[Number(m) - 1] ?? ""}`;
}

export function SessionsBarChart({
  data,
  minCount = 5,
  title = "Sessions per day",
}: {
  data: DayValue[];
  minCount?: number;
  title?: string;
}) {
  const uid = useId().replace(/:/g, "");
  const [active, setActive] = useState<number | null>(null);
  const known = data.map((d) => d.sessions).filter((v): v is number => v !== null);
  const peak = known.length ? Math.max(...known) : 0;
  const yMax = niceMax(Math.max(peak, minCount));
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const n = Math.max(data.length, 1);
  const slot = innerW / n;
  const gap = Math.min(2, slot * 0.2);
  const barW = Math.max(slot - gap, 1);
  const y = (v: number) => PAD.top + innerH - (v / yMax) * innerH;
  const labelEvery = Math.max(1, Math.ceil(n / 8));
  const suppressed = data.filter((d) => d.sessions === null).length;
  const total = known.reduce((a, b) => a + b, 0);
  const peakDay = data.find((d) => d.sessions === peak && peak > 0);
  const summary =
    data.length === 0
      ? "No days in range."
      : `${data.length} days from ${data[0]?.day} to ${data[data.length - 1]?.day}. ` +
        `Visible total ${total}${suppressed ? `, plus ${suppressed} day(s) with fewer than ${minCount} sessions hidden` : ""}. ` +
        (peakDay ? `Highest: ${peak} on ${peakDay.day}.` : "No sessions recorded.");
  const act = active !== null ? data[active] : undefined;
  const ticks = [0, yMax / 2, yMax];

  return (
    <figure className="chart">
      <figcaption>
        <strong>{title}</strong>
        <span className="chart-readout" aria-live="polite">
          {act
            ? `${act.day}: ${fmtCount(act.sessions, minCount)} sessions`
            : "Hover a bar for its value"}
        </span>
      </figcaption>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        role="img"
        aria-labelledby={`${uid}-t ${uid}-d`}
        preserveAspectRatio="xMidYMid meet"
        className="chart-svg"
        data-testid="sessions-chart"
      >
        <title id={`${uid}-t`}>{title}</title>
        <desc id={`${uid}-d`}>{summary}</desc>
        <defs>
          <pattern
            id={`${uid}-hatch`}
            width="6"
            height="6"
            patternUnits="userSpaceOnUse"
            patternTransform="rotate(45)"
          >
            <line x1="0" y1="0" x2="0" y2="6" className="chart-hatch" strokeWidth="2" />
          </pattern>
        </defs>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={PAD.left} x2={W - PAD.right} y1={y(t)} y2={y(t)} className="chart-grid" />
            <text x={PAD.left - 6} y={y(t)} dy="0.32em" textAnchor="end" className="chart-axis">
              {t}
            </text>
          </g>
        ))}
        {data.map((d, i) => {
          const x = PAD.left + i * slot + gap / 2;
          const isNull = d.sessions === null;
          // A hidden count is 1–4: draw a hatched bar up to the threshold so it
          // reads as "small, exact value withheld", never as zero.
          const v = isNull ? minCount - 1 : (d.sessions ?? 0);
          const top = y(v);
          const h = Math.max(PAD.top + innerH - top, v > 0 ? 1 : 0);
          const r = Math.min(4, barW / 2, h);
          const base = PAD.top + innerH;
          const path =
            h <= 0
              ? ""
              : `M${x},${base} V${top + r} Q${x},${top} ${x + r},${top} H${x + barW - r} Q${x + barW},${top} ${x + barW},${top + r} V${base} Z`;
          return (
            // biome-ignore lint/a11y/noStaticElementInteractions: hover readout only; values are in the table below
            <g
              key={d.day}
              onMouseEnter={() => setActive(i)}
              onMouseLeave={() => setActive(null)}
              data-testid="chart-bar"
              data-suppressed={isNull ? "true" : undefined}
            >
              <title>{`${d.day}: ${isNull ? `fewer than ${minCount} (hidden)` : `${d.sessions} sessions`}`}</title>
              <rect
                x={PAD.left + i * slot}
                y={PAD.top}
                width={slot}
                height={innerH}
                fill="transparent"
              />
              {path && (
                <path
                  d={path}
                  className={
                    isNull
                      ? "chart-bar-suppressed"
                      : active === i
                        ? "chart-bar chart-bar-active"
                        : "chart-bar"
                  }
                  fill={isNull ? `url(#${uid}-hatch)` : undefined}
                />
              )}
              {i % labelEvery === 0 && (
                <text
                  x={x + barW / 2}
                  y={H - PAD.bottom + 16}
                  textAnchor="middle"
                  className="chart-axis"
                >
                  {shortDay(d.day)}
                </text>
              )}
            </g>
          );
        })}
        <line
          x1={PAD.left}
          x2={W - PAD.right}
          y1={PAD.top + innerH}
          y2={PAD.top + innerH}
          className="chart-baseline"
        />
      </svg>
      {suppressed > 0 && (
        <p className="muted small">
          Hatched bars: fewer than {minCount} sessions that day; the exact value is withheld for
          privacy.
        </p>
      )}
      <details className="chart-table">
        <summary>Show data table</summary>
        <table>
          <caption className="sr-only">{title}</caption>
          <thead>
            <tr>
              <th scope="col">Day (IST)</th>
              <th scope="col">Sessions</th>
            </tr>
          </thead>
          <tbody>
            {data.map((d) => (
              <tr key={d.day}>
                <td>{d.day}</td>
                <td className="num">{fmtCount(d.sessions, minCount)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </figure>
  );
}
