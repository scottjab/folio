// Typed client for the folio API.
//
// There is no auth here on purpose: the tailnet authenticates the connection,
// so requests carry no token or cookie. What they do carry is same-origin
// credentials and the browser's Sec-Fetch-Site header, which is what the
// server's CSRF check relies on.

export interface Me {
  login: string;
  displayName: string;
  profilePic?: string;
  vault: string;
  isAgent: boolean;
}

export interface NoteSummary {
  vault: string;
  ownerLogin: string;
  path: string;
  title: string;
  tags: string[];
  sha256: string;
  updatedAt: string;
}

export interface Backlink {
  path: string;
  title: string;
  kind: string;
  alias?: string;
  anchor?: string;
}

export interface Note extends NoteSummary {
  content: string;
  size: number;
  modTime: string;
  perm: "read" | "write" | "";
  backlinks: Backlink[];
}

export interface SearchHit extends NoteSummary {
  snippet: string;
  score: number;
}

export interface VaultInfo {
  vault: string;
  ownerLogin: string;
  isMine: boolean;
}

export interface AttachmentInfo {
  path: string;
  size: number;
}

/** Where the server filed an uploaded file. */
export interface Upload {
  vault: string;
  path: string;
  size: number;
  sha256: string;
  /**
   * What to write inside [[ ]] to reference the file: the bare filename when
   * that is unambiguous in the vault, the full path when it is not. The server
   * decides this so the browser and the terminal client cannot disagree.
   */
  link: string;
}

/** A resolved ![[embed]]. */
export interface Embed {
  kind: "note" | "attachment" | "missing";
  vault: string;
  path?: string;
  title?: string;
  anchor?: string;
  content?: string;
  truncated?: boolean;
}

/** Settings that follow a user between the browser and the terminal. */
export interface Prefs {
  /** "vault", "folder", "current", or "subfolder", by Obsidian's names. */
  attachmentMode: string;
  attachmentFolder: string;
}

export interface ShareInfo {
  id: string;
  vault: string;
  ownerLogin: string;
  path: string;
  isFolder: boolean;
  granteeLogin: string;
  perm: "read" | "write";
  createdAt: string;
}

/** A save that lost a race. The rejected draft is kept at conflictPath. */
export class ConflictError extends Error {
  constructor(
    message: string,
    readonly conflictPath: string,
  ) {
    super(message);
    this.name = "ConflictError";
  }
}

/** Any non-2xx response, carrying the status so callers can branch on it. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
  });

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let body: any = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = { error: text };
    }
  }

  if (!res.ok) {
    const message = body?.error ?? `${res.status} ${res.statusText}`;
    if (res.status === 409 && body?.conflictPath) {
      throw new ConflictError(message, body.conflictPath);
    }
    throw new ApiError(message, res.status);
  }
  return body as T;
}

const enc = (p: string) => p.split("/").map(encodeURIComponent).join("/");

export const api = {
  me: () => request<Me>("/api/me"),

  vaults: () => request<{ vaults: VaultInfo[] }>("/api/vaults").then((r) => r.vaults),

  listNotes: (vault: string, opts: { folder?: string; tag?: string } = {}) => {
    const q = new URLSearchParams();
    if (opts.folder) q.set("folder", opts.folder);
    if (opts.tag) q.set("tag", opts.tag);
    const qs = q.toString();
    return request<{ notes: NoteSummary[] }>(
      `/api/vaults/${enc(vault)}/notes${qs ? "?" + qs : ""}`,
    ).then((r) => r.notes);
  },

  readNote: (vault: string, path: string) =>
    request<Note>(`/api/vaults/${enc(vault)}/notes/${enc(path)}`),

  createNote: (vault: string, path: string, content: string) =>
    request<NoteSummary>(`/api/vaults/${enc(vault)}/notes`, {
      method: "POST",
      body: JSON.stringify({ path, content }),
    }),

  /**
   * Saves a note. baseSha turns the write into a compare-and-swap: if the file
   * changed underneath us, the server refuses and parks our draft in a sibling
   * file rather than overwriting whatever arrived first.
   */
  saveNote: (vault: string, path: string, content: string, baseSha: string) =>
    request<NoteSummary>(`/api/vaults/${enc(vault)}/notes/${enc(path)}`, {
      method: "PUT",
      headers: baseSha ? { "If-Match": `"${baseSha}"` } : {},
      body: JSON.stringify({ content }),
    }),

  deleteNote: (vault: string, path: string) =>
    request<void>(`/api/vaults/${enc(vault)}/notes/${enc(path)}`, { method: "DELETE" }),

  moveNote: (vault: string, from: string, to: string) =>
    request<NoteSummary>(`/api/vaults/${enc(vault)}/move`, {
      method: "POST",
      body: JSON.stringify({ from, to }),
    }),

  dailyNote: (vault: string, date?: string) => {
    const q = date ? `?date=${encodeURIComponent(date)}` : "";
    return request<Note>(`/api/vaults/${enc(vault)}/daily${q}`);
  },

  search: (q: string, limit = 50) =>
    request<{ hits: SearchHit[]; hasMore: boolean }>(
      `/api/search?q=${encodeURIComponent(q)}&limit=${limit}`,
    ),

  tags: () => request<{ tags: Array<{ tag: string; count: number }> }>("/api/tags").then((r) => r.tags),

  folders: () => request<{ folders: string[] }>("/api/folders").then((r) => r.folders),

  users: () =>
    request<{ users: Array<{ login: string; displayName: string }> }>("/api/users").then(
      (r) => r.users,
    ),

  shares: () => request<{ shares: ShareInfo[] }>("/api/shares").then((r) => r.shares),

  sharedWithMe: () => request<{ shares: ShareInfo[] }>("/api/shared").then((r) => r.shares),

  share: (path: string, grantee: string, perm: "read" | "write", isFolder = false) =>
    request<ShareInfo>("/api/shares", {
      method: "POST",
      body: JSON.stringify({ path, grantee, perm, isFolder }),
    }),

  unshare: (id: string) => request<void>(`/api/shares/${encodeURIComponent(id)}`, { method: "DELETE" }),

  attachmentURL: (vault: string, path: string) =>
    `/api/vaults/${enc(vault)}/attachments/${enc(path)}`,

  listAttachments: (vault: string) =>
    request<{ attachments: AttachmentInfo[] }>(`/api/vaults/${enc(vault)}/attachments`).then(
      (r) => r.attachments,
    ),

  /**
   * Uploads a dropped or pasted file.
   *
   * The destination folder is deliberately not ours to choose: we say which note
   * we are inserting into and what the file was called, and the server applies
   * the user's attachment preference. Passing an empty name asks for a
   * "Pasted image ..." name, which is what a clipboard image should get.
   */
  upload: async (vault: string, note: string, name: string, file: Blob) => {
    const q = new URLSearchParams();
    if (note) q.set("note", note);
    if (name) q.set("name", name);
    const res = await fetch(`/api/vaults/${enc(vault)}/upload?${q}`, {
      method: "POST",
      headers: { "Content-Type": file.type || "application/octet-stream" },
      body: file,
    });
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`;
      try {
        message = JSON.parse(await res.text())?.error ?? message;
      } catch {
        // A non-JSON error body is still worth failing on, just less legibly.
      }
      throw new ApiError(message, res.status);
    }
    return (await res.json()) as Upload;
  },

  embed: (vault: string, from: string, target: string) => {
    const q = new URLSearchParams({ target });
    if (from) q.set("from", from);
    return request<Embed>(`/api/vaults/${enc(vault)}/embed?${q}`);
  },

  prefs: () => request<Prefs>("/api/prefs"),

  setPrefs: (p: Prefs) =>
    request<Prefs>("/api/prefs", { method: "PUT", body: JSON.stringify(p) }),
};

/** One change event from the server's SSE stream. */
export interface NoteEvent {
  id: string;
  kind: "note.created" | "note.updated" | "note.deleted" | "note.moved";
  vault: string;
  path: string;
  oldPath?: string;
  sha256?: string;
  byLogin?: string;
  at: string;
}

/**
 * Subscribes to note changes.
 *
 * This is what makes an edit in Obsidian, or by an agent over MCP, show up in an
 * open tab. EventSource reconnects on its own, so a dropped connection heals
 * without the app noticing.
 */
export function subscribe(onEvent: (e: NoteEvent) => void): () => void {
  const es = new EventSource("/api/events");
  const handler = (ev: MessageEvent) => {
    try {
      onEvent(JSON.parse(ev.data) as NoteEvent);
    } catch {
      // A frame we cannot parse is not worth breaking the stream over.
    }
  };
  for (const kind of ["note.created", "note.updated", "note.deleted", "note.moved"]) {
    es.addEventListener(kind, handler as EventListener);
  }
  return () => es.close();
}
