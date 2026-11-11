// Camera for the document check. Opened only after the agent accepted the
// citizen's consent (POST camera-consent); video only, never audio. The
// stream is stopped right after a photo is taken and whenever the document
// screen closes. The photo is a downscaled JPEG held in memory and sent
// once to the device agent; nothing is stored in the browser.

import { MAX_IMAGE_BYTES, MAX_IMAGE_SIDE } from "../api/client";

export interface Camera {
  /** getUserMedia, video only. Rejects with CameraError. */
  open(): Promise<MediaStream>;
  /** Grab the current frame of `video` as a JPEG ≤ 2 MB, ≤ 1600 px. */
  capture(video: HTMLVideoElement): Promise<Blob>;
}

export type CameraErrorCode = "denied" | "unavailable" | "capture";

export class CameraError extends Error {
  readonly code: CameraErrorCode;
  constructor(code: CameraErrorCode, message: string) {
    super(message);
    this.name = "CameraError";
    this.code = code;
  }
}

/** Stop every track of a stream (turns the camera light off). */
export function stopStream(stream: MediaStream | null | undefined): void {
  if (!stream) return;
  for (const track of stream.getTracks()) track.stop();
}

/** Scale (w, h) to fit within maxSide on the long side; never upscale. */
export function fitWithin(w: number, h: number, maxSide = MAX_IMAGE_SIDE): [number, number] {
  if (w <= 0 || h <= 0) return [0, 0];
  const scale = Math.min(1, maxSide / Math.max(w, h));
  return [Math.max(1, Math.round(w * scale)), Math.max(1, Math.round(h * scale))];
}

/** JPEG qualities tried in order until the photo fits MAX_IMAGE_BYTES. */
export const JPEG_QUALITIES = [0.85, 0.7, 0.55, 0.4] as const;

export async function encodeJpeg(
  canvas: HTMLCanvasElement,
  maxBytes = MAX_IMAGE_BYTES,
): Promise<Blob> {
  for (const q of JPEG_QUALITIES) {
    const blob = await new Promise<Blob | null>((resolve) =>
      canvas.toBlob((b) => resolve(b), "image/jpeg", q),
    );
    if (blob && blob.size <= maxBytes) return blob;
  }
  throw new CameraError("capture", "photo too large even at low quality");
}

export class BrowserCamera implements Camera {
  constructor(
    private readonly media: MediaDevices | undefined = globalThis.navigator?.mediaDevices,
  ) {}

  async open(): Promise<MediaStream> {
    if (!this.media?.getUserMedia) throw new CameraError("unavailable", "no camera API");
    try {
      return await this.media.getUserMedia({
        audio: false,
        video: {
          facingMode: { ideal: "environment" },
          width: { ideal: MAX_IMAGE_SIDE },
          height: { ideal: 1200 },
        },
      });
    } catch (e) {
      const name = e instanceof DOMException ? e.name : "";
      if (name === "NotAllowedError" || name === "SecurityError") {
        throw new CameraError("denied", "camera permission denied");
      }
      throw new CameraError("unavailable", name || "camera unavailable");
    }
  }

  async capture(video: HTMLVideoElement): Promise<Blob> {
    const [w, h] = fitWithin(video.videoWidth, video.videoHeight);
    if (w === 0 || h === 0) throw new CameraError("capture", "no video frame yet");
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new CameraError("capture", "no 2d context");
    ctx.drawImage(video, 0, 0, w, h);
    try {
      return await encodeJpeg(canvas);
    } finally {
      // Drop the pixels from the canvas as well.
      canvas.width = 0;
      canvas.height = 0;
    }
  }
}
