// Debounced autosave.
//
// Two timers, for the same reason the server's file watcher has two: the idle
// timer keeps us from saving on every keystroke, and the maximum keeps a long
// stretch of continuous typing from going unsaved indefinitely.
//
// This is deliberately a small pure class with an injected timer, so its
// behaviour can be tested without a browser or a clock.

export interface AutosaveOptions {
  /** Quiet period after the last keystroke before saving. */
  idleMs: number;
  /** Longest a pending change may wait, however continuously you type. */
  maxMs: number;
  save(): void | Promise<void>;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
}

export class Autosave {
  private handle: unknown = null;
  private firstPendingAt = 0;
  private dirty = false;

  private readonly now: () => number;
  private readonly setTimer: (fn: () => void, ms: number) => unknown;
  private readonly clearTimer: (h: unknown) => void;

  constructor(private opts: AutosaveOptions) {
    this.now = opts.now ?? (() => Date.now());
    this.setTimer = opts.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
    this.clearTimer = opts.clearTimer ?? ((h) => clearTimeout(h as ReturnType<typeof setTimeout>));
  }

  /** Records a change and (re)arms the timers. */
  schedule() {
    if (!this.dirty) {
      this.dirty = true;
      this.firstPendingAt = this.now();
    }
    this.rearm();
  }

  private rearm() {
    if (this.handle !== null) this.clearTimer(this.handle);

    const sinceFirst = this.now() - this.firstPendingAt;
    const untilMax = Math.max(this.opts.maxMs - sinceFirst, 0);
    const wait = Math.min(this.opts.idleMs, untilMax);

    this.handle = this.setTimer(() => {
      this.handle = null;
      this.flush();
    }, wait);
  }

  /** Saves now if anything is pending. */
  flush() {
    if (this.handle !== null) {
      this.clearTimer(this.handle);
      this.handle = null;
    }
    if (!this.dirty) return;
    this.dirty = false;
    this.opts.save();
  }

  /** Drops any pending save, used when navigating away from a note. */
  cancel() {
    if (this.handle !== null) {
      this.clearTimer(this.handle);
      this.handle = null;
    }
    this.dirty = false;
  }

  get pending(): boolean {
    return this.dirty;
  }
}
