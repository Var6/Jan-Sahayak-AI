// Display helpers. They only reformat what the agent sent; they never add
// information.

const DEVANAGARI_DIGITS = "०१२३४५६७८९";

/** Convert Devanagari digits to ASCII digits. */
export function asciiDigits(s: string): string {
  return s.replace(/[०-९]/g, (d) => String(DEVANAGARI_DIGITS.indexOf(d)));
}

/**
 * Format an ISO date ("2026-03-12" or RFC 3339) as DD-MM-YYYY. Anything
 * else is returned unchanged so no information is lost or invented.
 */
export function formatDate(iso: string | undefined): string | null {
  if (!iso) return null;
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso.trim());
  if (!m) return iso;
  return `${m[3]}-${m[2]}-${m[1]}`;
}

/**
 * Pull phone-number-like sequences (3–13 digits, optionally separated by
 * spaces or hyphens) out of a crisis reply so they can be shown very large.
 * Bounded: at most `max` distinct numbers.
 */
export function extractNumbers(text: string, max = 4): string[] {
  const out: string[] = [];
  const re = /(?<![\d०-९])[\d०-९](?:[\d०-९]|[ -](?=[\d०-९])){2,16}(?![\d०-९])/g;
  for (const m of text.matchAll(re)) {
    const raw = m[0];
    const digits = asciiDigits(raw).replace(/\D/g, "");
    if (digits.length < 3 || digits.length > 13) continue;
    const shown = asciiDigits(raw).replace(/ +/g, " ").trim();
    if (!out.includes(shown)) out.push(shown);
    if (out.length >= max) break;
  }
  return out;
}
