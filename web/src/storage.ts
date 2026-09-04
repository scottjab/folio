// Preferences that live on the device rather than on the server.
//
// Everything real is stored server-side; this is for the handful of choices that
// belong to a browser rather than to an account, such as how wide the text runs
// on this screen.

/**
 * Reads a saved preference.
 *
 * localStorage throws rather than returning null in a private window or a
 * browser with site data blocked, and losing a layout preference is not worth
 * taking the app down for.
 */
export function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export function writeStored(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Nothing to do: the preference simply will not survive a reload.
  }
}
