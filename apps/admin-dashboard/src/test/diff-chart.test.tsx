import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { fillDays, niceMax, SessionsBarChart } from "@/components/BarChart";
import { DiffTable } from "@/components/VersionDiff";
import { diffJson, MAX_DIFF_ENTRIES } from "@/lib/diff";
import { fmtCount } from "@/lib/format";

describe("diffJson", () => {
  it("reports added, removed and changed leaves with paths", () => {
    const a = {
      id: "x",
      name: { hi: "क", en: "A" },
      steps: [{ hi: "1" }, { hi: "2" }],
      helpline: "123",
    };
    const b = {
      id: "x",
      name: { hi: "क", en: "B" },
      steps: [{ hi: "1" }],
      official_url: "https://a.gov.in",
    };
    expect(diffJson(a, b)).toEqual([
      { path: "helpline", kind: "removed", before: "123" },
      { path: "name.en", kind: "changed", before: "A", after: "B" },
      { path: "official_url", kind: "added", after: "https://a.gov.in" },
      { path: "steps[1]", kind: "removed", before: { hi: "2" } },
    ]);
  });
  it("treats null vs [] as a change and identical values as none", () => {
    expect(diffJson({ a: null }, { a: [] })).toEqual([
      { path: "a", kind: "changed", before: null, after: [] },
    ]);
    expect(diffJson({ a: [1, { b: 2 }] }, { a: [1, { b: 2 }] })).toEqual([]);
  });
  it("is bounded", () => {
    const big = Array.from({ length: MAX_DIFF_ENTRIES + 50 }, (_, i) => i);
    expect(diffJson({ a: big }, { a: big.map((x) => x + 1) })).toHaveLength(MAX_DIFF_ENTRIES);
  });
  it("renders a table with before/after", () => {
    render(<DiffTable before={{ a: "old" }} after={{ a: "new" }} />);
    expect(screen.getByText("old").tagName).toBe("DEL");
    expect(screen.getByText("new").tagName).toBe("INS");
    render(<DiffTable before={{ a: 1 }} after={{ a: 1 }} />);
    expect(screen.getByText("No differences.")).toBeTruthy();
  });
});

describe("sessions chart", () => {
  it("fills missing days with zero and keeps suppressed days null", () => {
    const d = fillDays(
      [
        { day: "2026-11-01", sessions: 12 },
        { day: "2026-11-03", sessions: null },
      ],
      "2026-11-01",
      "2026-11-04",
    );
    expect(d).toEqual([
      { day: "2026-11-01", sessions: 12 },
      { day: "2026-11-02", sessions: 0 },
      { day: "2026-11-03", sessions: null },
      { day: "2026-11-04", sessions: 0 },
    ]);
  });
  it("picks nice axis maxima", () => {
    expect(niceMax(0)).toBe(5);
    expect(niceMax(7)).toBe(10);
    expect(niceMax(12)).toBe(20);
    expect(niceMax(450)).toBe(500);
  });
  it("renders an accessible SVG with one bar per day and a data table", () => {
    render(
      <SessionsBarChart
        data={[
          { day: "2026-11-01", sessions: 12 },
          { day: "2026-11-02", sessions: 0 },
          { day: "2026-11-03", sessions: null },
        ]}
      />,
    );
    const svg = screen.getByRole("img", { name: /sessions per day/i });
    expect(svg.querySelector("desc")?.textContent).toMatch(
      /3 days.*Visible total 12.*1 day\(s\) with fewer than 5.*Highest: 12 on 2026-11-01/,
    );
    const bars = screen.getAllByTestId("chart-bar");
    expect(bars).toHaveLength(3);
    expect(bars[2]?.getAttribute("data-suppressed")).toBe("true");
    expect(bars[1]?.querySelector("path")).toBeNull(); // zero draws no bar
    expect(screen.getByText(/Hatched bars/)).toBeTruthy();
    const cells = screen.getAllByRole("cell").map((c) => c.textContent);
    expect(cells).toEqual(["2026-11-01", "12", "2026-11-02", "0", "2026-11-03", "<5"]);
  });
  it("never shows a suppressed count as zero", () => {
    expect(fmtCount(null)).toBe("<5");
    expect(fmtCount(0)).toBe("0");
  });
});
