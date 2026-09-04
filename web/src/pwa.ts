// Installing folio, and keeping an installed copy up to date.
//
// This module is the browser-facing half of the progressive web app; sw.ts is
// the other half. It is kept out of app.ts because none of it has an opinion
// about notes: it reports events (an update is waiting, the network went away,
// this browser could install us) and leaves the app to decide what to show.
//
// Every entry point here is a no-op on a browser that lacks the API in
// question, so callers never have to feature-detect.

import { readStored, writeStored } from "./storage";

/** What the app wants to hear about. */
export interface PwaHandlers {
  /**
   * A newer build has downloaded and is waiting. Calling apply() switches to it
   * and reloads, so it belongs behind a button rather than being done for the
   * user: the reload throws away whatever is in the editor that autosave has
   * not flushed yet.
   */
  onUpdateReady(apply: () => void): void;

  /**
   * The connection came or went. Fired only on a change, never for the initial
   * state, which the caller can read from navigator.onLine itself.
   */
  onConnectionChange(online: boolean): void;

  /**
   * folio can be installed to the home screen or dock from here. `prompt` opens
   * the browser's own install dialog where there is one; on iOS there is not,
   * and it is null, which is what `hint` is for.
   */
  onInstallable(offer: InstallOffer): void;
}

/** An opportunity to install, in whatever form this browser offers one. */
export interface InstallOffer {
  /** Opens the browser's install dialog, or null on a browser without one. */
  prompt: (() => void) | null;
  /** What to tell someone who has to do it by hand. Empty when prompt is set. */
  hint: string;
}

/** Where the "you can install this" hint records that it has been seen. */
const INSTALL_HINT_KEY = "folio.installHintDismissed";

/**
 * How long to leave between asking the server whether sw.js has changed.
 *
 * An installed app is resumed rather than reloaded, sometimes dozens of times a
 * day, and each resume is a chance to notice a deploy. Without a floor that
 * would be dozens of requests for a file that changes weekly.
 */
const UPDATE_CHECK_MS = 15 * 60 * 1000;

/**
 * Registers the service worker and wires up the update flow.
 *
 * Registration failure is swallowed on purpose. A worker is an enhancement: the
 * app works without one, and a browser that refuses to register it (an insecure
 * origin, storage disabled) should not get an error banner about a feature it
 * was never going to use.
 */
export function registerServiceWorker(handlers: Pick<PwaHandlers, "onUpdateReady">) {
  if (!("serviceWorker" in navigator)) return;

  // Set only by apply() below. Without it the first install would reload the
  // page too, because clients.claim() in the worker fires controllerchange when
  // an uncontrolled page first gets a controller.
  let expectingReload = false;
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    if (!expectingReload) return;
    expectingReload = false;
    window.location.reload();
  });

  void navigator.serviceWorker
    .register("/sw.js")
    .then((reg) => {
      let announced = false;
      const announce = (worker: ServiceWorker) => {
        if (announced) return;
        announced = true;
        handlers.onUpdateReady(() => {
          expectingReload = true;
          worker.postMessage({ type: "skip-waiting" });
        });
      };

      // Already waiting: the update installed during an earlier visit and the
      // tab was closed before it was taken up.
      if (reg.waiting && navigator.serviceWorker.controller) announce(reg.waiting);

      reg.addEventListener("updatefound", () => {
        const installing = reg.installing;
        if (!installing) return;
        installing.addEventListener("statechange", () => {
          // A controller means this is an update rather than the very first
          // install, which nobody needs to be told about.
          if (installing.state === "installed" && navigator.serviceWorker.controller) {
            announce(installing);
          }
        });
      });

      watchForUpdates(reg);
    })
    .catch(() => {
      // See the note above: a worker is an enhancement.
    });
}

/** Asks the browser to recheck sw.js whenever the app comes back to the front. */
function watchForUpdates(reg: ServiceWorkerRegistration) {
  let last = Date.now();
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") return;
    if (Date.now() - last < UPDATE_CHECK_MS) return;
    last = Date.now();
    void reg.update().catch(() => {});
  });
}

/**
 * Reports connection changes.
 *
 * navigator.onLine is a weak signal: it means "this device has a network", not
 * "folio is reachable", and on a tailnet the two come apart regularly. It is
 * still worth reporting, because the case it does catch, a phone that has left
 * wifi, is the one where an unexplained failure is most confusing.
 *
 * Returns a function that stops the reporting.
 */
export function watchConnection(onChange: (online: boolean) => void): () => void {
  const online = () => onChange(true);
  const offline = () => onChange(false);
  window.addEventListener("online", online);
  window.addEventListener("offline", offline);
  return () => {
    window.removeEventListener("online", online);
    window.removeEventListener("offline", offline);
  };
}

/**
 * Reports whether this page is running as an installed app.
 *
 * Two checks, because iOS predates the standard one and still answers only to
 * navigator.standalone.
 */
export function isStandalone(): boolean {
  const legacy = (navigator as Navigator & { standalone?: boolean }).standalone;
  if (legacy) return true;
  return window.matchMedia?.("(display-mode: standalone)").matches === true;
}

/**
 * Offers to install folio, once, on a browser where that means something.
 *
 * Chromium fires beforeinstallprompt and hands over a dialog we can open on a
 * click. Safari fires nothing and has no API at all: adding to the home screen
 * is a manual trip through the share sheet, so all we can do there is say so,
 * and iOS is the platform where it matters most, since a home-screen folio is
 * the only way to get the app full screen with its own icon.
 *
 * Either way it is said once and then remembered as dismissed, because a nag
 * about installing an app you are already using is the worst thing a web app
 * does.
 */
export function offerInstall(onInstallable: (offer: InstallOffer) => void) {
  if (isStandalone()) return;
  if (readStored(INSTALL_HINT_KEY) === "1") return;

  window.addEventListener("beforeinstallprompt", (e) => {
    // Suppressing the browser's own mini-infobar is the price of being allowed
    // to call prompt() later.
    e.preventDefault();
    const deferred = e as BeforeInstallPromptEvent;
    dismissInstallHint();
    onInstallable({
      prompt: () => void deferred.prompt(),
      hint: "",
    });
  });

  if (isIOSSafari()) {
    dismissInstallHint();
    onInstallable({
      prompt: null,
      hint: "Add folio to your home screen from the share menu, and it opens full screen with its own icon.",
    });
  }
}

/** Records that the install hint has been shown, so it is shown only once. */
function dismissInstallHint() {
  writeStored(INSTALL_HINT_KEY, "1");
}

/**
 * Reports whether this is Safari on iOS or iPadOS.
 *
 * User-agent sniffing, which is always a bit wrong, and is used here only to
 * decide whether to print one sentence. iPadOS reports itself as a Mac, so the
 * touch-point count is what separates an iPad from a laptop; Chrome and Firefox
 * on iOS are WebKit underneath but cannot add to the home screen at all, so they
 * are excluded by name.
 */
function isIOSSafari(): boolean {
  const ua = navigator.userAgent;
  const iOS = /iPad|iPhone|iPod/.test(ua) ||
    (ua.includes("Macintosh") && navigator.maxTouchPoints > 1);
  if (!iOS) return false;
  return !/CriOS|FxiOS|EdgiOS|OPiOS/.test(ua);
}

/** The Chromium-only event, which TypeScript's DOM library does not declare. */
interface BeforeInstallPromptEvent extends Event {
  prompt(): Promise<void>;
}
