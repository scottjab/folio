import { describe, expect, it } from "vitest";
import { Autosave } from "./autosave";

/** A fake clock and timer, so these tests are deterministic. */
class Clock {
  time = 0;
  private timers: Array<{ at: number; fn: () => void; id: number }> = [];
  private nextId = 1;

  now = () => this.time;

  setTimer = (fn: () => void, ms: number) => {
    const id = this.nextId++;
    this.timers.push({ at: this.time + ms, fn, id });
    return id;
  };

  clearTimer = (h: unknown) => {
    this.timers = this.timers.filter((t) => t.id !== h);
  };

  advance(ms: number) {
    const target = this.time + ms;
    for (;;) {
      const next = this.timers
        .filter((t) => t.at <= target)
        .sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      this.timers = this.timers.filter((t) => t.id !== next.id);
      this.time = next.at;
      next.fn();
    }
    this.time = target;
  }
}

function setup(idleMs = 800, maxMs = 5000) {
  const clock = new Clock();
  const saves: number[] = [];
  const autosave = new Autosave({
    idleMs,
    maxMs,
    save: () => {
      saves.push(clock.time);
    },
    now: clock.now,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  });
  return { clock, saves, autosave };
}

describe("Autosave", () => {
  it("saves once after the typing stops", () => {
    const { clock, saves, autosave } = setup();
    for (let i = 0; i < 5; i++) {
      autosave.schedule();
      clock.advance(100);
    }
    clock.advance(800);
    expect(saves).toEqual([1200]);
  });

  it("does not save while typing continues within the idle window", () => {
    const { clock, saves, autosave } = setup();
    autosave.schedule();
    clock.advance(700);
    expect(saves).toHaveLength(0);
  });

  it("saves at the maximum even under continuous typing", () => {
    // Someone typing steadily must not have their work sit unsaved forever.
    const { clock, saves, autosave } = setup(800, 2000);
    for (let i = 0; i < 40; i++) {
      autosave.schedule();
      clock.advance(200);
    }
    expect(saves.length).toBeGreaterThanOrEqual(3);
    expect(saves[0]).toBeLessThanOrEqual(2000);
  });

  it("does nothing when nothing changed", () => {
    const { clock, saves } = setup();
    clock.advance(10_000);
    expect(saves).toEqual([]);
  });

  it("flush saves immediately", () => {
    const { clock, saves, autosave } = setup();
    autosave.schedule();
    autosave.flush();
    expect(saves).toEqual([clock.time]);
  });

  it("flush is a no-op when nothing is pending", () => {
    const { saves, autosave } = setup();
    autosave.flush();
    autosave.flush();
    expect(saves).toEqual([]);
  });

  it("cancel drops the pending save", () => {
    // Navigating away from a note must not save it into the next one.
    const { clock, saves, autosave } = setup();
    autosave.schedule();
    autosave.cancel();
    clock.advance(10_000);
    expect(saves).toEqual([]);
  });

  it("reports whether a save is pending", () => {
    const { autosave } = setup();
    expect(autosave.pending).toBe(false);
    autosave.schedule();
    expect(autosave.pending).toBe(true);
    autosave.flush();
    expect(autosave.pending).toBe(false);
  });

  it("starts a fresh window after a save", () => {
    const { clock, saves, autosave } = setup();
    autosave.schedule();
    clock.advance(1000);
    expect(saves).toHaveLength(1);

    autosave.schedule();
    clock.advance(1000);
    expect(saves).toHaveLength(2);
  });
});
