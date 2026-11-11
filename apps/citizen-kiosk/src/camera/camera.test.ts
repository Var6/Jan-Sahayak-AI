import { describe, expect, it, vi } from "vitest";
import { MAX_IMAGE_BYTES } from "../api/client";
import { BrowserCamera, CameraError, encodeJpeg, fitWithin, stopStream } from "./camera";

describe("fitWithin", () => {
  it("downscales the long side to 1600 px and never upscales", () => {
    expect(fitWithin(3200, 2400)).toEqual([1600, 1200]);
    expect(fitWithin(2400, 3200)).toEqual([1200, 1600]);
    expect(fitWithin(1280, 720)).toEqual([1280, 720]);
    expect(fitWithin(1601, 1)).toEqual([1600, 1]);
    expect(fitWithin(0, 720)).toEqual([0, 0]);
  });
});

describe("stopStream", () => {
  it("stops every track", () => {
    const tracks = [{ stop: vi.fn() }, { stop: vi.fn() }];
    stopStream({ getTracks: () => tracks } as unknown as MediaStream);
    for (const t of tracks) expect(t.stop).toHaveBeenCalledTimes(1);
    stopStream(null); // no-op
  });
});

describe("encodeJpeg", () => {
  it("lowers the quality until the photo fits 2 MB", async () => {
    const qualities: number[] = [];
    const sizes = [MAX_IMAGE_BYTES + 1, MAX_IMAGE_BYTES + 1, 1000];
    const canvas = {
      toBlob: (cb: (b: Blob | null) => void, type: string, q: number) => {
        expect(type).toBe("image/jpeg");
        qualities.push(q);
        cb(new Blob([new Uint8Array(sizes[qualities.length - 1] ?? 10)], { type }));
      },
    } as unknown as HTMLCanvasElement;
    const blob = await encodeJpeg(canvas);
    expect(blob.size).toBe(1000);
    expect(qualities).toEqual([0.85, 0.7, 0.55]);
  });

  it("gives up when even the lowest quality is too large", async () => {
    const canvas = {
      toBlob: (cb: (b: Blob | null) => void) => cb(new Blob([new Uint8Array(20)])),
    } as unknown as HTMLCanvasElement;
    await expect(encodeJpeg(canvas, 10)).rejects.toBeInstanceOf(CameraError);
  });
});

describe("BrowserCamera.open", () => {
  it("asks for video only, never audio", async () => {
    const getUserMedia = vi.fn().mockResolvedValue({ getTracks: () => [] });
    const cam = new BrowserCamera({ getUserMedia } as unknown as MediaDevices);
    await cam.open();
    const constraints = getUserMedia.mock.calls[0]?.[0] as MediaStreamConstraints;
    expect(constraints.audio).toBe(false);
    expect(constraints.video).toBeTruthy();
  });

  it("maps permission errors", async () => {
    const denied = new BrowserCamera({
      getUserMedia: vi.fn().mockRejectedValue(new DOMException("no", "NotAllowedError")),
    } as unknown as MediaDevices);
    await expect(denied.open()).rejects.toMatchObject({ code: "denied" });
    const missing = new BrowserCamera({
      getUserMedia: vi.fn().mockRejectedValue(new DOMException("no", "NotFoundError")),
    } as unknown as MediaDevices);
    await expect(missing.open()).rejects.toMatchObject({ code: "unavailable" });
    await expect(new BrowserCamera(undefined).open()).rejects.toMatchObject({
      code: "unavailable",
    });
  });
});
