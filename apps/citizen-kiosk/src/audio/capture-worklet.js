// AudioWorklet processor: forwards mono input to the main thread in batches
// of ~2048 samples (fewer messages than the 128-frame render quantum).
// Plain JS: it is loaded as a separate module by audioWorklet.addModule().
const BATCH = 2048;

class CaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buf = new Float32Array(BATCH);
    this.fill = 0;
    this.port.onmessage = (e) => {
      if (e.data === "flush" && this.fill > 0) {
        this.port.postMessage(this.buf.slice(0, this.fill));
        this.fill = 0;
      }
    };
  }

  process(inputs) {
    const ch = inputs[0]?.[0];
    if (ch) {
      let i = 0;
      while (i < ch.length) {
        const n = Math.min(ch.length - i, BATCH - this.fill);
        this.buf.set(ch.subarray(i, i + n), this.fill);
        this.fill += n;
        i += n;
        if (this.fill === BATCH) {
          this.port.postMessage(this.buf.slice());
          this.fill = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor("js-capture", CaptureProcessor);
