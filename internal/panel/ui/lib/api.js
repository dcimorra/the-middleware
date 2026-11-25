// The API client. One place knows the base path, the header that every
// non-GET must carry and what a 401 means, so a screen never has to.

import { nodePath } from "./node.js";

export const BASE = "/api/v1";

// apiURL is the address of path on node (the current one when undefined),
// for what the browser fetches by itself: a link, an image, a frame.
export function apiURL(path, node) { return BASE + nodePath(path, node); }

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

const listeners = new Set();

// onUnauthorized is how the shell learns the cookie died while a screen was
// open: the request that noticed it fails, and the shell swaps in the login
// screen instead of every screen handling 401 on its own.
export function onUnauthorized(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// signalUnauthorized is the same news from something that learned it
// without a 401 of its own: the event stream asks /auth/me, which is
// public and answers 200 with authenticated false.
export function signalUnauthorized() {
  for (const fn of listeners) fn();
}

async function request(method, path, body, opts = {}) {
  const headers = { Accept: "application/json" };
  const init = { method, headers, credentials: "same-origin", signal: opts.signal };
  if (method !== "GET" && method !== "HEAD") {
    // The API refuses state changes without this header: a form on another
    // origin cannot set it, which is what keeps the cookie from being
    // ridden by a page the operator happens to have open.
    headers["X-Requested-With"] = "middleware";
  }
  if (body !== undefined) {
    if (opts.raw) {
      headers["Content-Type"] = opts.contentType || "application/octet-stream";
      init.body = body;
    } else {
      headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
  }
  const res = await fetch(apiURL(path, opts.node), init);
  if (res.status === 401 && !opts.allow401) signalUnauthorized();
  const ct = res.headers.get("content-type") || "";
  if (!res.ok) {
    let message = res.statusText || `HTTP ${res.status}`;
    let parsed = null;
    try {
      if (ct.includes("json")) {
        parsed = await res.json();
        if (parsed && parsed.error) message = parsed.error;
      } else {
        const text = await res.text();
        if (text) message = text.slice(0, 500);
      }
    } catch { /* the status is the message then */ }
    throw new ApiError(res.status, message, parsed);
  }
  if (opts.text) return res.text();
  if (res.status === 204) return null;
  if (ct.includes("json")) return res.json();
  return res.text();
}

export const api = {
  get: (path, opts) => request("GET", path, undefined, opts),
  text: (path, opts) => request("GET", path, undefined, { ...opts, text: true }),
  post: (path, body, opts) => request("POST", path, body ?? {}, opts),
  put: (path, body, opts) => request("PUT", path, body, opts),
  patch: (path, body, opts) => request("PATCH", path, body, opts),
  del: (path, opts) => request("DELETE", path, undefined, opts),
  putRaw: (path, body, contentType) => request("PUT", path, body, { raw: true, contentType }),
  postRaw: (path, body, contentType) => request("POST", path, body, { raw: true, contentType }),
  // at is the same client on one node, whichever the panel is on: the
  // dashboard and the sites list ask every node at once.
  at: (node) => ({
    get: (path, opts) => request("GET", path, undefined, { ...opts, node }),
    post: (path, body, opts) => request("POST", path, body ?? {}, { ...opts, node }),
    put: (path, body, opts) => request("PUT", path, body, { ...opts, node }),
    del: (path, opts) => request("DELETE", path, undefined, { ...opts, node }),
  }),
  // stream opens a GET whose body keeps coming (logs with ?follow=1) and
  // hands each chunk to onChunk until the controller aborts it.
  async stream(path, onChunk, signal) {
    const res = await fetch(apiURL(path), { credentials: "same-origin", signal });
    if (!res.ok) throw new ApiError(res.status, await res.text().catch(() => res.statusText));
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      onChunk(dec.decode(value, { stream: true }));
    }
  },
};

export function enc(s) { return encodeURIComponent(s); }

export function qs(params) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v === undefined || v === null || v === "") continue;
    p.set(k, String(v));
  }
  const s = p.toString();
  return s ? "?" + s : "";
}
