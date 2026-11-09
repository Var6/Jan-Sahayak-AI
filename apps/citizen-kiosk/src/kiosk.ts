// Kiosk hardening: no context menu, no pinch/ctrl-wheel zoom, no drag of
// images/text. Chromium's --kiosk flags cover the rest (no URL bar, etc.).

export function hardenKiosk(win: Window): () => void {
  const opts: AddEventListenerOptions = { passive: false };
  const prevent = (e: Event) => e.preventDefault();
  const onWheel = (e: WheelEvent) => {
    if (e.ctrlKey) e.preventDefault();
  };
  const onTouchMove = (e: TouchEvent) => {
    if (e.touches.length > 1) e.preventDefault();
  };
  const onKey = (e: KeyboardEvent) => {
    // Block browser zoom shortcuts (Ctrl/Cmd + +/-/0).
    if ((e.ctrlKey || e.metaKey) && ["+", "-", "=", "0"].includes(e.key)) e.preventDefault();
  };
  const d = win.document;
  d.addEventListener("contextmenu", prevent, opts);
  d.addEventListener("dragstart", prevent, opts);
  d.addEventListener("gesturestart", prevent, opts);
  d.addEventListener("wheel", onWheel, opts);
  d.addEventListener("touchmove", onTouchMove, opts);
  d.addEventListener("keydown", onKey, opts);
  return () => {
    d.removeEventListener("contextmenu", prevent);
    d.removeEventListener("dragstart", prevent);
    d.removeEventListener("gesturestart", prevent);
    d.removeEventListener("wheel", onWheel);
    d.removeEventListener("touchmove", onTouchMove);
    d.removeEventListener("keydown", onKey);
  };
}
