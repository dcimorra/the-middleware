// The icon set: drawn here, on a 24-unit grid, 1.75 stroke, round caps,
// one path string each. Hand-made rather than a bundled icon font so the
// panel stays a handful of files with no build step and no third-party
// asset: an icon is a string in this table and nothing else. Every icon
// is decorative (aria-hidden); the word next to it carries the meaning,
// and where an icon stands alone the button's aria-label does.

import { svg } from "./dom.js";

const PATHS = {
  // navigation
  layout: "M4 4h7v9H4zM13 4h7v5h-7zM13 11h7v9h-7zM4 15h7v5H4z",
  server: "M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01",
  globe: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 0c-2.5 2.6-3.75 5.6-3.75 9s1.25 6.4 3.75 9m0-18c2.5 2.6 3.75 5.6 3.75 9s-1.25 6.4-3.75 9M3.5 9h17M3.5 15h17",
  door: "M14 4h5a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-5M4 12h10m-3.5-3.5L14 12l-3.5 3.5",
  shield: "M12 3 4.5 6v5.5c0 4.6 3.1 8 7.5 9.5 4.4-1.5 7.5-4.9 7.5-9.5V6L12 3Zm-3 9 2 2 4-4.5",
  blocks: "M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z",
  clock: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 4.5V12l3.5 2",
  activity: "M3 12h3.5l2.5-7 4 14 2.5-7H21",
  chart: "M4 20h16M7 16v-4m5 4V6m5 10v-7",
  eye: "M2.5 12s3.5-6.5 9.5-6.5 9.5 6.5 9.5 6.5-3.5 6.5-9.5 6.5S2.5 12 2.5 12Zm9.5-2.5a2.5 2.5 0 1 0 0 5 2.5 2.5 0 0 0 0-5Z",
  eyeOff: "M3.5 3.5l17 17M10.4 5.7c.5-.1 1-.2 1.6-.2 6 0 9.5 6.5 9.5 6.5a16.5 16.5 0 0 1-2.9 3.7M6.6 6.7C4 8.4 2.5 12 2.5 12s3.5 6.5 9.5 6.5c1.8 0 3.4-.6 4.8-1.4M10.2 10.2a2.5 2.5 0 0 0 3.6 3.6",
  sliders: "M4 7h9m4 0h3M4 17h3m4 0h9M13 4.5v5M7 14.5v5",
  cpu: "M7 7h10v10H7zM4 10h3m-3 4h3m10-4h3m-3 4h3M10 4v3m4-3v3m-4 10v3m4-3v3",
  logout: "M10 4H6a1 1 0 0 0-1 1v14a1 1 0 0 0 1 1h4m4-4 4-4-4-4m4 4H9",
  // chrome
  menu: "M4 6h16M4 12h16M4 18h16",
  more: "M5 12h.01M12 12h.01M19 12h.01",
  search: "M10.5 4a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13Zm5 11.5L20 20",
  sun: "M12 4v1.5M12 18.5V20M4 12h1.5M18.5 12H20M6.3 6.3l1.1 1.1M16.6 16.6l1.1 1.1M6.3 17.7l1.1-1.1M16.6 7.4l1.1-1.1M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8Z",
  moon: "M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5Z",
  monitor: "M4 5h16v11H4zM9 20h6m-3-4v4",
  help: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm-2.5 6.5a2.5 2.5 0 1 1 3.6 2.2c-.7.4-1.1.9-1.1 1.8M12 17h.01",
  x: "M6 6l12 12M18 6 6 18",
  check: "M5 12.5 10 17.5 19 7",
  minus: "M6 12h12",
  plus: "M12 5v14M5 12h14",
  chevronRight: "m9 6 6 6-6 6",
  chevronDown: "m6 9 6 6 6-6",
  external: "M14 4h6v6m0-6-9 9M11 5H6a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-5",
  // actions
  refresh: "M20 11A8 8 0 0 0 5.6 6.6L4 8m0-4v4h4m-4 5a8 8 0 0 0 14.4 4.4L20 16m0 4v-4h-4",
  play: "M8 5.5v13l10-6.5-10-6.5Z",
  stop: "M6 6h12v12H6z",
  pause: "M8 5v14m8-14v14",
  trash: "M4 7h16M9 7V4h6v3m-7 0 .7 13h6.6L16 7",
  copy: "M9 9h11v11H9zM5 15H4V4h11v1",
  edit: "m4 20 4-1L19.5 7.5l-3-3L5 16l-1 4Zm10.5-13.5 3 3",
  save: "M5 4h11l3 3v13H5zM8 4v5h7V4M8 20v-6h8v6",
  upload: "M12 16V5m-5 5 5-5 5 5M4 19h16",
  download: "M12 4v11m-5-5 5 5 5-5M4 19h16",
  send: "M4 12 20 4l-4 16-4-7-8-1Z",
  arrowUp: "M12 19V5m-6.5 6.5L12 5l6.5 6.5",
  arrowDown: "M12 5v14m-6.5-6.5L12 19l6.5-6.5",
  chevronDown: "m6 9 6 6 6-6",
  chevronRight: "m9 6 6 6-6 6",
  compose: "M11 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-5M18.5 3.5a2.1 2.1 0 0 1 3 3L13 15l-4 1 1-4 8.5-8.5Z",
  paperclip: "M20 11.5 12.2 19.3a5 5 0 0 1-7.1-7.1l8.1-8.1a3.3 3.3 0 0 1 4.7 4.7l-8.1 8.1a1.7 1.7 0 0 1-2.4-2.4l7.4-7.4",
  lock: "M7 11V8a5 5 0 0 1 10 0v3M5 11h14v9H5zm7 4v2",
  unlock: "M7 11V8a5 5 0 0 1 9.6-2M5 11h14v9H5zm7 4v2",
  key: "M14.5 3.5a6 6 0 0 0-5.7 7.9L3 17.2V21h3.8l1.4-1.4V17h2.6l1.6-1.6a6 6 0 1 0 2.1-11.9Zm2 4a1 1 0 1 1 0 2 1 1 0 0 1 0-2Z",
  terminal: "m5 7 5 5-5 5m8 0h6",
  chat: "M4 5h16v11h-9l-4 3.5V16H4z",
  file: "M6 3h8l4 4v14H6zm8 0v4h4",
  folder: "M3 6h7l2 2h9v11H3z",
  code: "m8 8-4 4 4 4m8-8 4 4-4 4m-6 2 4-12",
  list: "M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01",
  alert: "M12 4 2.5 20h19L12 4Zm0 6v4m0 3h.01",
  info: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 8v5m0-8h.01",
  bell: "M6 16V11a6 6 0 0 1 12 0v5l1.5 2h-15L6 16Zm4 4h4",
  box: "M12 3 3.5 7.5v9L12 21l8.5-4.5v-9L12 3Zm0 9L3.5 7.5M12 12l8.5-4.5M12 12v9",
  branch: "M6 4v16M6 8a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm0 16a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm12-12a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm0 0v2a4 4 0 0 1-4 4H6",
  link: "M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1.5 1.5M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1.5-1.5",
  power: "M12 3v9m6.4-5.4a9 9 0 1 1-12.8 0",
  wifi: "M3 9.5a14 14 0 0 1 18 0M6.5 13a9 9 0 0 1 11 0M10 16.5a4 4 0 0 1 4 0M12 20h.01",
  ban: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm-6.4 2.6 12.8 12.8",
  sparkles: "M12 4 13.8 10.2 20 12l-6.2 1.8L12 20l-1.8-6.2L4 12l6.2-1.8L12 4ZM5 4l.7 2.3L8 7l-2.3.7L5 10l-.7-2.3L2 7l2.3-.7L5 4Z",
  timer: "M12 6a7 7 0 1 0 0 14 7 7 0 0 0 0-14Zm0 3.5V13l2.5 1.5M9 3h6",
  gauge: "M4 15a8 8 0 1 1 16 0M12 15l4-5m-4 5h.01",
  history: "M4 12a8 8 0 1 0 2.3-5.6L4 8.7M4 4v4.7h4.7M12 8v4l3 2",
};

// icon returns a 20px (by default) inline SVG for a name in the table. An
// unknown name renders nothing rather than throwing, so a screen written
// against a newer table degrades to its words.
export function icon(name, opts = {}) {
  const d = PATHS[name];
  if (!d) return document.createTextNode("");
  const size = opts.size || 20;
  const el = svg("svg", {
    viewBox: "0 0 24 24", width: size, height: size, fill: "none", stroke: "currentColor",
    "stroke-width": opts.stroke || 1.75, "stroke-linecap": "round", "stroke-linejoin": "round",
    "aria-hidden": "true", class: "ico" + (opts.class ? " " + opts.class : ""),
  }, svg("path", { d }));
  return el;
}
