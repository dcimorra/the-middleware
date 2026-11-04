// The site's resources on its page, as three bars read at a glance: how
// much memory it may hold, how many cores it may take, and how it ranks
// against the other sites when they compete. The scale of each bar is the
// machine (GET /sites/{d}/limits), its far right means "no limit", and
// what the site uses now is said under it, so a ceiling is never set
// blind. Saving writes the manifest's `limits` (the owner's alone: a
// delegate cannot raise what its site takes from the machine), applied
// live by the house.
//
// The bars move in steps rather than bytes: "1.5 GB" is a decision, and
// 1 563 914 240 is a slip of the thumb.

import { h, clear, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice } from "../components/page.js";
import { toast, toastError } from "../components/toast.js";

const MiB = 1 << 20;
const GiB = 1 << 30;
const MEMORY_STEPS = [128, 256, 384, 512, 768, 1024, 1536, 2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768, 49152, 65536, 98304, 131072].map((m) => m * MiB);
const CPU_STEPS = [0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64];
// cpu_weight on the cgroup scale, 100 an equal share with every site.
const WEIGHTS = [25, 50, 100, 200, 400];

// parseSize reads a size as the manifest writes it ("512m", "1.5g").
export function parseSize(s) {
  if (s === undefined || s === null || s === "") return 0;
  const m = String(s).trim().toLowerCase().match(/^(\d+(?:\.\d+)?)\s*([kmgt]?)i?b?$/);
  if (!m) return 0;
  const mult = { "": 1, k: 1024, m: MiB, g: GiB, t: 1024 * GiB }[m[2]];
  return Math.round(Number(m[1]) * mult);
}

// sizeText writes bytes back the way an operator would: whole gigabytes
// as "2g", the rest in megabytes.
export function sizeText(n) {
  return n % GiB === 0 ? `${n / GiB}g` : `${Math.round(n / MiB)}m`;
}

// nearest is the index of the step closest to v, for a limit written by
// hand that is not on the bar's steps.
function nearest(steps, v) {
  let best = 0;
  for (let i = 1; i < steps.length; i++) if (Math.abs(steps[i] - v) < Math.abs(steps[best] - v)) best = i;
  return best;
}

export function resourcesCard(domain) {
  const el = h("section", { class: "card site-section site-res" });
  const path = `/sites/${enc(domain)}/limits`;
  let data = null;
  let last = null; // the last CPU reading: {usec, at}
  let cpuNow = null; // cores in use, between the last two readings
  let dirty = false;
  // Polled while it is on the page, as the cluster card is: the overview
  // rebuilds around it and takes it away when the site's page closes.
  let seen = false;
  // The sliders' state: an index into the steps; the last index of the
  // memory and CPU bars is "no limit".
  let memIdx = 0, cpuIdx = 0, wIdx = 2;
  let memSteps = [], cpuSteps = [];

  function fromLimits(l) {
    l = l || {};
    const mem = parseSize(l.memory);
    memIdx = mem ? nearest(memSteps, mem) : memSteps.length;
    cpuIdx = l.cpus ? nearest(cpuSteps, l.cpus) : cpuSteps.length;
    wIdx = nearest(WEIGHTS, l.cpu_weight || 100);
  }

  // The limits as the bars say them, over what the manifest has: the
  // fields this card does not draw (memory_high, pids) are kept, and a
  // squeeze point left above a lowered ceiling goes, or the save fails.
  function toLimits() {
    const out = { ...((data && data.limits) || {}) };
    delete out.memory; delete out.cpus; delete out.cpu_weight;
    if (memIdx < memSteps.length) {
      out.memory = sizeText(memSteps[memIdx]);
      if (out.memory_high && parseSize(out.memory_high) > memSteps[memIdx]) delete out.memory_high;
    }
    if (cpuIdx < cpuSteps.length) out.cpus = cpuSteps[cpuIdx];
    if (WEIGHTS[wIdx] !== 100) out.cpu_weight = WEIGHTS[wIdx];
    return Object.keys(out).length ? out : null;
  }

  // where is the position on a bar of a value between its steps (log
  // between two neighbours, as the steps grow): the mark of what the site
  // uses now, so the thumb is seen against it.
  function where(steps, v) {
    if (!v || !steps.length) return null;
    if (v <= steps[0]) return 0;
    for (let i = 1; i < steps.length; i++) {
      if (v <= steps[i]) return i - 1 + Math.log(v / steps[i - 1]) / Math.log(steps[i] / steps[i - 1]);
    }
    return steps.length;
  }

  function bar({ label, hint, steps, index, unlimited, text, onInput, mark }) {
    const value = h("strong", { class: "site-res-value" });
    const input = h("input", { type: "range", min: "0", max: String(steps), step: "1", value: String(index), "aria-label": label });
    const track = h("div", { class: "site-res-track" }, input);
    if (mark !== null && mark !== undefined && steps > 0) {
      // The thumb's centre travels inside the track less its own width.
      const m = h("span", { class: "site-res-mark", title: mark.title });
      m.style.left = `calc(9px + (100% - 18px) * ${Math.min(1, mark.at / steps)})`;
      track.append(m);
    }
    const paintValue = () => { value.textContent = text(Number(input.value)); };
    input.addEventListener("input", () => { onInput(Number(input.value)); dirty = true; paintValue(); paintFoot(); });
    paintValue();
    return h("div", { class: "site-res-bar" },
      h("div", { class: "site-res-head" }, h("span", null, label), value),
      track,
      h("div", { class: "site-res-scale small muted" }, h("span", null, unlimited[0]), h("span", null, unlimited[1])),
      hint);
  }

  const memNote = h("div", { class: "small" });
  const cpuNote = h("div", { class: "small muted" });
  const foot = h("div", { class: "form-actions" });
  const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
  const reset = h("button", { type: "button", class: "btn" }, t("res.reset"));
  foot.append(save, reset);

  function paintMemNote() {
    clear(memNote);
    const use = data && data.use;
    if (!use) return;
    const parts = [t("res.memUse", { now: fmtBytes(use.memory), peak: fmtBytes(use.peak) })];
    memNote.className = "small muted";
    if (memIdx < memSteps.length && use.peak && memSteps[memIdx] < use.peak * 1.25) {
      memNote.className = "small level-warn";
      parts.push(t("res.memTight"));
    }
    if (use.oom_kills) parts.push(t("res.memKills", { n: use.oom_kills }));
    memNote.textContent = parts.join(" ");
  }
  function paintCpuNote() {
    cpuNote.textContent = cpuNow === null ? "" : t("res.cpuUse", { n: cpuNow.toFixed(cpuNow < 1 ? 2 : 1) });
  }
  function paintFoot() {
    paintMemNote();
    save.disabled = !dirty;
    reset.disabled = !dirty;
  }

  function draw() {
    const m = data.machine || {};
    memSteps = MEMORY_STEPS.filter((s) => s < (m.memory || 0));
    cpuSteps = CPU_STEPS.filter((c) => c < (m.cpus || 1));
    if (!dirty) fromLimits(data.limits);
    const weightWord = (i) => t("res.weight." + i);
    const use = data.use || null;
    clear(el).append(...[
      h("h2", null, t("res.title")),
      h("p", { class: "small muted" }, t("res.hint")),
      data.applied ? null : notice("warn", t("res.notApplied")),
      bar({
        label: t("res.memory"), steps: memSteps.length, index: memIdx,
        unlimited: [fmtBytes(memSteps[0] || 0), t("res.noLimit")],
        text: (i) => (i >= memSteps.length ? t("res.noLimitMem", { total: fmtBytes(m.memory) }) : fmtBytes(memSteps[i])),
        onInput: (i) => { memIdx = i; },
        mark: use && use.peak ? { at: where(memSteps, use.peak), title: t("res.markPeak", { n: fmtBytes(use.peak) }) } : null,
        hint: h("div", null, memNote, h("div", { class: "small muted" }, t("res.memHandover"))),
      }),
      bar({
        label: t("res.cpus"), steps: cpuSteps.length, index: cpuIdx,
        unlimited: [t("res.cores", { n: cpuSteps[0] || "" }), t("res.noLimit")],
        text: (i) => (i >= cpuSteps.length ? t("res.noLimitCpu", { n: m.cpus }) : t("res.cores", { n: cpuSteps[i] })),
        onInput: (i) => { cpuIdx = i; },
        mark: cpuNow ? { at: where(cpuSteps, cpuNow), title: t("res.cpuUse", { n: cpuNow.toFixed(2) }) } : null,
        hint: h("div", null, cpuNote, h("div", { class: "small muted" }, t("res.cpusHint"))),
      }),
      bar({
        label: t("res.weight"), steps: WEIGHTS.length - 1, index: wIdx,
        unlimited: [weightWord(0), weightWord(WEIGHTS.length - 1)],
        text: (i) => weightWord(i),
        onInput: (i) => { wIdx = i; },
        hint: h("div", { class: "small muted" }, t("res.weightHint")),
      }),
      foot].filter(Boolean));
    paintCpuNote();
    paintFoot();
  }

  async function load() {
    try { data = await api.get(path); } catch { el.hidden = true; return; }
    el.hidden = false;
    const use = data.use;
    if (use) {
      const now = { usec: use.cpu_usec, at: performance.now() };
      if (last && now.at > last.at) cpuNow = Math.max(0, (now.usec - last.usec) / ((now.at - last.at) * 1000));
      last = now;
    }
    if (dirty && el.childElementCount) { paintMemNote(); paintCpuNote(); } else draw();
  }

  async function write(limits) {
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { limits });
      toast(t("res.saved"), "ok");
      dirty = false;
      await load();
    } catch (e) { toastError(e); }
  }
  save.addEventListener("click", () => write(toLimits()));
  reset.addEventListener("click", () => { dirty = false; draw(); });

  const tick = async () => {
    if (el.isConnected) seen = true;
    else if (seen) return;
    await load();
    setTimeout(tick, 5000);
  };
  tick();
  return el;
}
