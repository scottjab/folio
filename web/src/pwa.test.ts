import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { isStandalone, offerInstall, registerServiceWorker, watchConnection } from "./pwa";

/**
 * The update flow is the part of a progressive web app that is easiest to get
 * quietly wrong, and hardest to notice: a reload at the wrong moment throws away
 * what someone was typing, and no reload at all leaves an installed folio
 * running last month's bundle forever with no way to say so.
 */

/** A service worker double, with the handful of methods the module uses. */
class FakeWorker extends EventTarget {
  state = "installing";
  posted: unknown[] = [];
  postMessage(msg: unknown) {
    this.posted.push(msg);
  }
  become(state: string) {
    this.state = state;
    this.dispatchEvent(new Event("statechange"));
  }
}

class FakeRegistration extends EventTarget {
  installing: FakeWorker | null = null;
  waiting: FakeWorker | null = null;
  updates = 0;
  update() {
    this.updates++;
    return Promise.resolve();
  }
  /** Mimics the browser finding a new worker and installing it. */
  install(): FakeWorker {
    const worker = new FakeWorker();
    this.installing = worker;
    this.dispatchEvent(new Event("updatefound"));
    worker.become("installed");
    return worker;
  }
}

class FakeContainer extends EventTarget {
  controller: unknown = null;
  registered: string[] = [];
  constructor(private reg: FakeRegistration) {
    super();
  }
  register(url: string) {
    this.registered.push(url);
    return Promise.resolve(this.reg as unknown as ServiceWorkerRegistration);
  }
}

let reg: FakeRegistration;
let container: FakeContainer;
let reload: ReturnType<typeof vi.fn>;

/** Installs the doubles. `controlled` is false on a browser's first ever visit. */
function stubServiceWorker(controlled = true) {
  reg = new FakeRegistration();
  container = new FakeContainer(reg);
  container.controller = controlled ? {} : null;
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    get: () => container,
  });
}

/** Lets the register() promise and its .then chain settle. */
const settle = () => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  reload = vi.fn();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...window.location, reload, search: "", pathname: "/" },
  });
  localStorage.clear();
});

afterEach(() => {
  Reflect.deleteProperty(navigator, "serviceWorker");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("registering the worker", () => {
  it("does nothing at all on a browser without service workers", () => {
    // Not an error state. folio works fine uninstalled, and an insecure origin
    // or a locked-down browser should not produce a banner about it.
    const onUpdateReady = vi.fn();
    expect(() => registerServiceWorker({ onUpdateReady })).not.toThrow();
    expect(onUpdateReady).not.toHaveBeenCalled();
  });

  it("registers at the root, so its scope covers every client-side route", async () => {
    stubServiceWorker();
    registerServiceWorker({ onUpdateReady: vi.fn() });
    await settle();
    expect(container.registered).toEqual(["/sw.js"]);
  });

  it("survives a registration the browser refuses", async () => {
    stubServiceWorker();
    vi.spyOn(container, "register").mockRejectedValue(new Error("no"));
    const onUpdateReady = vi.fn();

    registerServiceWorker({ onUpdateReady });
    await settle();
    expect(onUpdateReady).not.toHaveBeenCalled();
  });
});

describe("taking up a new version", () => {
  it("offers the update rather than applying it", async () => {
    // Reloading under someone mid-sentence loses whatever autosave has not
    // flushed. The app asks first, which is what the callback is for.
    stubServiceWorker();
    const onUpdateReady = vi.fn();
    registerServiceWorker({ onUpdateReady });
    await settle();

    reg.install();
    expect(onUpdateReady).toHaveBeenCalledTimes(1);
    expect(reload).not.toHaveBeenCalled();
  });

  it("switches to the new worker and reloads when the offer is accepted", async () => {
    stubServiceWorker();
    let apply = () => {};
    registerServiceWorker({ onUpdateReady: (a) => (apply = a) });
    await settle();
    const worker = reg.install();

    apply();
    expect(worker.posted).toEqual([{ type: "skip-waiting" }]);
    // The reload waits for the new worker to actually take over.
    expect(reload).not.toHaveBeenCalled();

    container.dispatchEvent(new Event("controllerchange"));
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("does not reload when a worker takes over on its own", async () => {
    // clients.claim() fires controllerchange the first time a page is claimed.
    // Reloading on that would restart the app under someone who never asked.
    stubServiceWorker(false);
    registerServiceWorker({ onUpdateReady: vi.fn() });
    await settle();

    container.dispatchEvent(new Event("controllerchange"));
    expect(reload).not.toHaveBeenCalled();
  });

  it("says nothing about the very first install", async () => {
    // With no controller there is no old version to replace, so there is
    // nothing to tell anyone about.
    stubServiceWorker(false);
    const onUpdateReady = vi.fn();
    registerServiceWorker({ onUpdateReady });
    await settle();

    reg.install();
    expect(onUpdateReady).not.toHaveBeenCalled();
  });

  it("picks up an update that installed during an earlier visit", async () => {
    stubServiceWorker();
    reg.waiting = new FakeWorker();
    const onUpdateReady = vi.fn();

    registerServiceWorker({ onUpdateReady });
    await settle();
    expect(onUpdateReady).toHaveBeenCalledTimes(1);
  });

  it("offers the same update only once", async () => {
    stubServiceWorker();
    reg.waiting = new FakeWorker();
    const onUpdateReady = vi.fn();

    registerServiceWorker({ onUpdateReady });
    await settle();
    reg.install();
    expect(onUpdateReady).toHaveBeenCalledTimes(1);
  });

  it("checks for a new version when the app comes back to the front", async () => {
    // An installed app is resumed rather than reloaded, so without this it could
    // run for weeks without ever asking whether it is current.
    stubServiceWorker();
    registerServiceWorker({ onUpdateReady: vi.fn() });
    await settle();

    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    vi.spyOn(Date, "now").mockReturnValue(Date.now() + 60 * 60 * 1000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(reg.updates).toBe(1);
  });

  it("does not ask again on every resume", async () => {
    stubServiceWorker();
    registerServiceWorker({ onUpdateReady: vi.fn() });
    await settle();

    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    document.dispatchEvent(new Event("visibilitychange"));
    expect(reg.updates).toBe(0);
  });
});

describe("the connection", () => {
  it("reports going offline and coming back", () => {
    const seen: boolean[] = [];
    const stop = watchConnection((online) => seen.push(online));

    window.dispatchEvent(new Event("offline"));
    window.dispatchEvent(new Event("online"));
    expect(seen).toEqual([false, true]);

    stop();
    window.dispatchEvent(new Event("offline"));
    expect(seen).toEqual([false, true]);
  });
});

describe("offering to install", () => {
  it("tells iOS how, since Safari has no install dialog", () => {
    // Chromium fires beforeinstallprompt and folio can open a real dialog.
    // Safari fires nothing, and adding to the home screen is a manual trip
    // through the share sheet, so the only thing to do is say so.
    vi.spyOn(navigator, "userAgent", "get").mockReturnValue(
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
    );
    const offers: Array<{ prompt: unknown; hint: string }> = [];

    offerInstall((o) => offers.push(o));
    expect(offers).toHaveLength(1);
    expect(offers[0].prompt).toBeNull();
    expect(offers[0].hint).toMatch(/home screen/i);
  });

  it("says it once and then never again", () => {
    vi.spyOn(navigator, "userAgent", "get").mockReturnValue(
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1",
    );
    const seen = vi.fn();

    offerInstall(seen);
    offerInstall(seen);
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("stays quiet in a browser that cannot add to the home screen", () => {
    // Chrome and Firefox on iOS are WebKit underneath but have no such menu
    // item, so the hint would be instructions for something you cannot do.
    vi.spyOn(navigator, "userAgent", "get").mockReturnValue(
      "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 CriOS/130.0 Mobile/15E148 Safari/604.1",
    );
    const seen = vi.fn();

    offerInstall(seen);
    expect(seen).not.toHaveBeenCalled();
  });

  it("hands over the browser's own dialog where there is one", () => {
    const prompt = vi.fn().mockResolvedValue(undefined);
    const offers: Array<{ prompt: (() => void) | null }> = [];
    offerInstall((o) => offers.push(o));

    const event = new Event("beforeinstallprompt");
    Object.assign(event, { prompt });
    window.dispatchEvent(event);

    expect(offers).toHaveLength(1);
    offers[0].prompt?.();
    expect(prompt).toHaveBeenCalled();
  });

  it("says nothing to an app that is already installed", () => {
    vi.stubGlobal("matchMedia", (q: string) => ({ matches: q.includes("standalone"), media: q }));
    const seen = vi.fn();

    expect(isStandalone()).toBe(true);
    offerInstall(seen);
    expect(seen).not.toHaveBeenCalled();
  });
});
