import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { RecordForm } from "@/components/RecordForm";
import { hasLiveVerified, isOwnEdit, validReviewDue } from "@/lib/review";
import {
  CATEGORIES,
  emptyRecord,
  isValidDate,
  parseRecordJson,
  RECORD_KEYS,
  validateRecord,
} from "@/lib/scheme";
import type { SchemeRecord, VersionMeta } from "@/lib/types";

// Vitest runs with the package directory as cwd.
const GO_SCHEME = resolve(process.cwd(), "../../internal/scheme/scheme.go");

/** A structurally valid dev record. FIXTURE: not a real scheme, no facts. */
function fixture(): SchemeRecord {
  return {
    ...emptyRecord(),
    id: "dev-fixture-scheme",
    name: { hi: "परीक्षण योजना (FIXTURE)", en: "Test scheme (FIXTURE)" },
    category: "pension",
    summary: { hi: "केवल परीक्षण के लिए।", en: "For tests only." },
    sources: [{ url: "https://example.gov.in/page", title: "Example", accessed_on: "2026-11-01" }],
  };
}

describe("validateRecord (mirror of scheme.Record.Validate)", () => {
  it("accepts a structurally valid record", () => {
    expect(validateRecord(fixture())).toEqual([]);
  });
  it("flags the essential rules", () => {
    const r = {
      ...fixture(),
      id: "Bad Id",
      kind: "" as const,
      name: { hi: "", en: "x" },
      category: "astrology",
      state: "UP",
      summary: { hi: "", en: "x" },
      official_url: "https://example.com/apply",
      helpline: "call me",
      sources: [{ url: "ftp://x", title: "", accessed_on: "2026-02-30" }],
    };
    const fields = validateRecord(r).map((i) => i.field);
    expect(fields).toEqual(
      expect.arrayContaining([
        "id",
        "kind",
        "name",
        "category",
        "state",
        "summary",
        "official_url",
        "helpline",
        "sources.0.url",
      ]),
    );
  });
  it("requires at least one source and real dates", () => {
    expect(validateRecord({ ...fixture(), sources: [] }).map((i) => i.field)).toContain("sources");
    const bad = {
      ...fixture(),
      sources: [{ url: "https://a.gov.in", title: "", accessed_on: "2026-13-01" }],
    };
    expect(validateRecord(bad).map((i) => i.field)).toContain("sources.0.accessed_on");
    expect(isValidDate("2024-02-29")).toBe(true);
    expect(isValidDate("2026-02-29")).toBe(false);
  });
  it("accepts government hosts for official_url only", () => {
    for (const ok of [
      "https://serviceonline.bihar.gov.in/x",
      "https://pmkisan.gov.in",
      "http://x.nic.in",
    ]) {
      expect(validateRecord({ ...fixture(), official_url: ok })).toEqual([]);
    }
    expect(
      validateRecord({ ...fixture(), official_url: "https://gov.in.evil.com" }).map((i) => i.field),
    ).toContain("official_url");
  });
  it("refuses changing the id of an existing scheme", () => {
    expect(validateRecord(fixture(), { pathId: "other-id" }).map((i) => i.field)).toContain("id");
  });
  it("keeps CATEGORIES and RECORD_KEYS in sync with the Go source", () => {
    const go = readFileSync(GO_SCHEME, "utf8");
    const block = /var Categories = \[\]string\{([\s\S]*?)\}/.exec(go)?.[1] ?? "";
    const goCats = [...block.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]);
    expect([...CATEGORIES]).toEqual(goCats);
    const recordBlock = /type Record struct \{([\s\S]*?)\n\}/.exec(go)?.[1] ?? "";
    const goKeys = [...recordBlock.matchAll(/json:"([a-z_]+)/g)].map((m) => m[1]);
    expect([...RECORD_KEYS].sort()).toEqual([...goKeys].sort());
  });
});

describe("parseRecordJson", () => {
  it("rejects invalid JSON, non-objects and unknown fields", () => {
    expect(parseRecordJson("{").error).toMatch(/Invalid JSON/);
    expect(parseRecordJson("[]").error).toMatch(/object/);
    expect(parseRecordJson('{"id":"x","status":"VERIFIED"}').error).toMatch(/status/);
    expect(parseRecordJson(JSON.stringify(fixture())).record?.id).toBe("dev-fixture-scheme");
  });
});

describe("review rules", () => {
  const h = (version: number, status: VersionMeta["status"], edited_by: string): VersionMeta => ({
    version,
    status,
    edited_by,
    edited_at: "2026-11-01T00:00:00Z",
  });
  it("four-eyes: any own draft since the last VERIFIED version blocks verification", () => {
    const hist = [
      h(4, "NEEDS_REVIEW", "bob"),
      h(3, "NEEDS_REVIEW", "alice"),
      h(2, "VERIFIED", "alice"),
      h(1, "NEEDS_REVIEW", "carol"),
    ];
    expect(isOwnEdit(hist, "alice")).toBe(true);
    expect(isOwnEdit(hist, "bob")).toBe(true);
    expect(isOwnEdit(hist, "carol")).toBe(false); // edited only before the last verification
    expect(isOwnEdit(hist, "dave")).toBe(false);
  });
  it("withdraw needs a live (not withdrawn) VERIFIED version", () => {
    expect(hasLiveVerified([h(1, "NEEDS_REVIEW", "a")])).toBe(false);
    expect(hasLiveVerified([h(2, "VERIFIED", "a"), h(1, "NEEDS_REVIEW", "a")])).toBe(true);
    expect(hasLiveVerified([h(3, "OUTDATED", "a"), h(2, "VERIFIED", "a")])).toBe(false);
    expect(
      hasLiveVerified([h(4, "VERIFIED", "a"), h(3, "OUTDATED", "a"), h(2, "VERIFIED", "a")]),
    ).toBe(true);
  });
  it("review_due must be a future date", () => {
    expect(validReviewDue("", "2026-11-11")).toBe(true);
    expect(validReviewDue("2026-11-11", "2026-11-11")).toBe(false);
    expect(validReviewDue("2027-01-01", "2026-11-11")).toBe(true);
  });
});

describe("<RecordForm>", () => {
  it("shows validation errors and does not save an invalid record", async () => {
    const onSave = vi.fn();
    render(<RecordForm initial={emptyRecord()} isNew onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /save draft/i }));
    const issues = await screen.findByTestId("form-issues");
    expect(issues.textContent).toMatch(/lowercase slug/);
    expect(issues.textContent).toMatch(/Hindi summary/);
    expect(onSave).not.toHaveBeenCalled();
  });

  it("saves a valid record with the change note", async () => {
    const onSave = vi.fn();
    render(<RecordForm initial={fixture()} isNew={false} onSave={onSave} />);
    await userEvent.type(screen.getByLabelText(/change note/i), "fix typo");
    await userEvent.click(screen.getByRole("button", { name: /save draft/i }));
    expect(onSave).toHaveBeenCalledTimes(1);
    const [rec, note] = onSave.mock.calls[0] as [SchemeRecord, string];
    expect(rec.id).toBe("dev-fixture-scheme");
    expect(note).toBe("fix typo");
    expect("helpline" in rec).toBe(false); // empty optional fields are omitted
  });

  it("round-trips through the raw JSON editor and refuses unknown fields", async () => {
    const onSave = vi.fn();
    render(<RecordForm initial={fixture()} isNew={false} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: "Raw JSON" }));
    const ta = screen.getByLabelText("Record JSON") as HTMLTextAreaElement;
    const obj = JSON.parse(ta.value);
    obj.bogus = 1;
    await userEvent.clear(ta);
    ta.focus();
    await userEvent.paste(JSON.stringify(obj));
    await userEvent.click(screen.getByRole("button", { name: /save draft/i }));
    expect(await screen.findByText(/Unknown field\(s\).*bogus/)).toBeTruthy();
    expect(onSave).not.toHaveBeenCalled();
    delete obj.bogus;
    obj.summary.en = "Edited in JSON";
    await userEvent.clear(ta);
    ta.focus();
    await userEvent.paste(JSON.stringify(obj));
    await userEvent.click(screen.getByRole("button", { name: "Form" }));
    expect((screen.getAllByDisplayValue("Edited in JSON")[0] as HTMLTextAreaElement).value).toBe(
      "Edited in JSON",
    );
  });
});
