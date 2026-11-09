// Plays reply audio served by the device agent. One clip at a time; every
// play has a timeout so a stalled clip can never wedge the conversation.

export type PlayOutcome = "ended" | "stopped" | "error" | "timeout";

export interface Player {
  play(url: string): Promise<PlayOutcome>;
  stop(): void;
  readonly playing: boolean;
}

/** Upper bound for one clip if the duration is unknown. */
const MAX_CLIP_MS = 90_000;
/** Allowance on top of the known duration (buffering, decode). */
const SLACK_MS = 5_000;

export class AudioPlayer implements Player {
  private el: HTMLAudioElement | null = null;
  private settle: ((o: PlayOutcome) => void) | null = null;

  get playing(): boolean {
    return this.settle !== null;
  }

  play(url: string): Promise<PlayOutcome> {
    this.stop();
    const el = this.el ?? new Audio();
    this.el = el;
    return new Promise<PlayOutcome>((resolve) => {
      let timer: ReturnType<typeof setTimeout> | null = null;
      const arm = (ms: number) => {
        if (timer) clearTimeout(timer);
        timer = setTimeout(() => done("timeout"), ms);
      };
      const done = (o: PlayOutcome) => {
        if (this.settle !== done) return;
        this.settle = null;
        if (timer) clearTimeout(timer);
        el.onended = null;
        el.onerror = null;
        el.onloadedmetadata = null;
        el.pause();
        resolve(o);
      };
      this.settle = done;
      el.onended = () => done("ended");
      el.onerror = () => done("error");
      el.onloadedmetadata = () => {
        if (Number.isFinite(el.duration) && el.duration > 0) {
          arm(Math.min(el.duration * 1000 + SLACK_MS, MAX_CLIP_MS + SLACK_MS));
        }
      };
      arm(MAX_CLIP_MS);
      el.preload = "auto";
      el.src = url;
      el.play().catch(() => done("error"));
    });
  }

  stop(): void {
    const s = this.settle;
    if (s) s("stopped");
    if (this.el) {
      this.el.removeAttribute("src");
      this.el.load(); // drop the decoded clip from memory
    }
  }
}
