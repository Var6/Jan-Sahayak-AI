// Self-hosted fonts (bundled; no network). Latin from Noto Sans, Devanagari
// from Noto Sans Devanagari; the browser picks per glyph.
import "@fontsource/noto-sans/latin-400.css";
import "@fontsource/noto-sans/latin-700.css";
import "@fontsource/noto-sans-devanagari/devanagari-400.css";
import "@fontsource/noto-sans-devanagari/devanagari-700.css";
import "./styles.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { DeviceApi } from "./api/client";
import { AudioPlayer } from "./audio/player";
import { MicRecorder } from "./audio/recorder";
import { hardenKiosk } from "./kiosk";

hardenKiosk(window);

const deps = {
  api: new DeviceApi(),
  recorder: new MicRecorder(),
  player: new AudioPlayer(),
};

const root = document.getElementById("root");
if (!root) throw new Error("missing #root");
createRoot(root).render(
  <StrictMode>
    <App deps={deps} />
  </StrictMode>,
);
