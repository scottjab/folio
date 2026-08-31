// Client-side routing.
//
// The server serves index.html for any unknown path, so these URLs survive a
// refresh and can be pasted to someone else on the tailnet.
//
//   /                          the app, showing today's daily note
//   /n/<vault>/<path>          a note
//   /t/<tag>                   notes carrying a tag
//   /s/<query>                 a search
export type Route =
  | { kind: "home" }
  | { kind: "note"; vault: string; path: string }
  | { kind: "tag"; tag: string }
  | { kind: "search"; query: string };

/** Parses a URL path into a route. Anything unrecognised is home. */
export function parseRoute(pathname: string): Route {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return { kind: "home" };

  switch (parts[0]) {
    case "n":
      if (parts.length >= 3) {
        return { kind: "note", vault: parts[1], path: parts.slice(2).join("/") };
      }
      return { kind: "home" };
    case "t":
      return parts.length >= 2 ? { kind: "tag", tag: parts.slice(1).join("/") } : { kind: "home" };
    case "s":
      return parts.length >= 2 ? { kind: "search", query: parts.slice(1).join("/") } : { kind: "home" };
    default:
      return { kind: "home" };
  }
}

/** Builds the URL for a route. */
export function routeToPath(route: Route): string {
  const enc = (s: string) => s.split("/").map(encodeURIComponent).join("/");
  switch (route.kind) {
    case "home":
      return "/";
    case "note":
      return `/n/${enc(route.vault)}/${enc(route.path)}`;
    case "tag":
      return `/t/${enc(route.tag)}`;
    case "search":
      return `/s/${enc(route.query)}`;
  }
}

/** Watches the address bar and reports the current route. */
export class Router {
  constructor(private onRoute: (r: Route) => void) {
    window.addEventListener("popstate", () => this.onRoute(this.current()));
  }

  current(): Route {
    return parseRoute(window.location.pathname);
  }

  /** Navigates, pushing history unless replace is set. */
  go(route: Route, replace = false) {
    const path = routeToPath(route);
    if (path === window.location.pathname) return;
    if (replace) history.replaceState(null, "", path);
    else history.pushState(null, "", path);
    this.onRoute(route);
  }

  start() {
    this.onRoute(this.current());
  }
}
