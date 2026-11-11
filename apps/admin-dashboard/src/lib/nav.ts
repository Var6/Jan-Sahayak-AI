/** Only same-app relative paths are accepted as a post-login destination. */
export function safeNext(next: string | null): string {
  if (!next?.startsWith("/") || next.startsWith("//") || next.includes("\\")) return "/";
  if (next.startsWith("/login")) return "/";
  return next;
}
