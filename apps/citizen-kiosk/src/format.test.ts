import { describe, expect, it } from "vitest";
import { asciiDigits, extractNumbers, formatDate } from "./format";

describe("formatDate", () => {
  it("formats ISO dates as DD-MM-YYYY and leaves anything else untouched", () => {
    expect(formatDate("2026-03-12")).toBe("12-03-2026");
    expect(formatDate("2026-03-12T10:00:00Z")).toBe("12-03-2026");
    expect(formatDate("March 2026")).toBe("March 2026");
    expect(formatDate(undefined)).toBeNull();
  });
});

describe("extractNumbers", () => {
  it("finds phone-like sequences, including spaced/hyphenated and Devanagari digits", () => {
    expect(extractNumbers("call 12345 or 011-2345 6789 now")).toEqual(["12345", "011-2345 6789"]);
    expect(extractNumbers("नंबर १२३४५ पर")).toEqual(["12345"]);
  });

  it("ignores short numbers, dedupes and bounds the result", () => {
    expect(extractNumbers("उम्र 70")).toEqual([]);
    expect(extractNumbers("111 111 222")).toEqual(["111 111 222"]);
    expect(extractNumbers("100, 100, 200, 300, 400, 500", 3)).toEqual(["100", "200", "300"]);
  });

  it("asciiDigits", () => {
    expect(asciiDigits("०१२३४५६७८९")).toBe("0123456789");
  });
});
