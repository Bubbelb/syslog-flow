const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

// Run the actual viewer script with a small DOM stand-in and controlled polls.
const source = fs.readFileSync("web.go", "utf8");
const script = source.slice(source.lastIndexOf("  <script>\n") + "  <script>\n".length,
  source.lastIndexOf("  </script>"));

class Element {
  constructor() {
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.listeners = {};
  }
  appendChild(child) {
    if (child.fragment) {
      for (const node of [...child.children]) this.appendChild(node);
    } else {
      child.parent = this;
      this.children.push(child);
    }
  }
  replaceChildren(fragment) {
    this.children = [];
    this.appendChild(fragment);
  }
  insertBefore(fragment) {
    const previous = this.children;
    this.children = [];
    this.appendChild(fragment);
    this.children.push(...previous);
  }
  remove() {
    this.parent.children.splice(this.parent.children.indexOf(this), 1);
  }
  querySelectorAll() { return [...this.children]; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  get scrollHeight() { return this.children.length * 20; }
  get clientHeight() { return this.height ?? 100; }
}

function viewerContext(day, layout) {
  const viewer = new Element();
  viewer.scrollTop = 0;
  viewer.dataset.refreshUrl = day ? "/day/2026/10/20?partial=logs&format=json" : "/?partial=logs&format=json";
  if (day) Object.assign(viewer.dataset, {
    olderUrl: viewer.dataset.refreshUrl, refreshCursor: "10", start: "0", hasOlder: "false",
  });
  const timers = [];
  const requests = [];
  let reloads = 0;
  let payload;
  const document = {
    body: { dataset: { generation: "generation-1" } },
    querySelectorAll: () => [],
    querySelector: () => null,
    getElementById: id => id === "log-viewer" ? viewer : null,
    createElement: () => new Element(),
    createTextNode: text => ({ textContent: text }),
    createDocumentFragment: () => Object.assign(new Element(), { fragment: true }),
  };
  const context = vm.createContext({
    document, URL,
    window: { location: { origin: "http://localhost", reload: () => { reloads++; } }, getSelection: () => null },
    requestAnimationFrame: callback => callback(),
    setInterval: callback => timers.push(callback),
    fetch: async url => {
      requests.push(String(url));
      return { ok: true, json: async () => payload };
    },
  });
  if (layout) {
    viewer.height = layout.viewerHeight;
    const classes = new Set();
    const panel = {
      querySelectorAll: () => layout.filters,
      classList: {
        contains: name => classes.has(name),
        toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name),
      },
    };
    viewer.closest = () => panel;
    context.ResizeObserver = class {
      constructor(callback) { layout.resize = callback; }
      observe() {}
    };
    layout.compact = () => classes.has("compact-filters");
  }
  vm.runInContext(script, context);
  return {
    viewer, requests, context,
    get reloads() { return reloads; },
    async poll(value) {
      payload = value;
      viewer.scrollTop = Math.max(0, viewer.scrollHeight - viewer.clientHeight);
      await timers[0]();
    },
    async pollStats(value) {
      payload = value;
      await timers[1]();
    },
    async older(value) {
      payload = value;
      viewer.scrollTop = 0;
      viewer.listeners.scroll();
      // The scroll listener starts an async pagination request.
      await new Promise(resolve => setImmediate(resolve));
    },
  };
}

async function main() {
  const layout = { viewerHeight: 180, filters: [{ scrollHeight: 280, clientHeight: 280 }] };
  const fitted = viewerContext(true, layout);
  assert.equal(layout.compact(), true, "pills must compact when the viewer is too short");
  layout.filters[0].clientHeight = 90;
  fitted.viewer.height = 370;
  layout.resize();
  assert.equal(layout.compact(), true, "reclaimed space must not cause an expand/collapse loop");
  fitted.viewer.height = 490;
  layout.resize();
  assert.equal(layout.compact(), true, "leave space above the collapse threshold to prevent boundary jitter");
  fitted.viewer.height = 530;
  layout.resize();
  assert.equal(layout.compact(), false, "pills should expand when enough viewer space remains");
  layout.filters[0].clientHeight = 280;
  fitted.viewer.height = 340;
  layout.resize();
  assert.equal(layout.compact(), false, "expanded layout must stay stable");
  fitted.viewer.height = 150;
  layout.resize();
  assert.equal(layout.compact(), true, "other controls growing must compact the pills again");
  layout.filters[0].scrollHeight = 90;
  layout.filters[0].clientHeight = 90;
  fitted.viewer.height = 340;
  layout.resize();
  assert.equal(layout.compact(), false, "wider layouts with fewer pill rows should expand again");

  const home = viewerContext(false);
  await home.poll({ lines: ["first log"] });
  assert.equal(home.viewer.children.length, 1, "an initially empty homepage must receive logs");

  const day = viewerContext(true);
  await day.poll({ lines: Array.from({ length: 6000 }, (_, i) => "log " + i), cursor: 6010 });
  assert.equal(day.viewer.children.length, 5000, "live rows must be bounded");
  assert.equal(day.viewer.children[0].dataset.raw, "log 1000");
  await day.poll({ lines: ["next log"], cursor: 6011 });
  assert.equal(day.viewer.children.length, 5000);
  assert.equal(day.viewer.children[0].dataset.raw, "log 1001");
  await day.older({ lines: ["older log"], start: 1000, hasMoreBefore: true });
  assert.equal(new URL(day.requests.at(-1), "http://localhost").searchParams.get("before"), "1001");
  assert.equal(day.viewer.children[0].dataset.raw, "older log", "trimmed history must remain accessible");

  await day.poll({ lines: ["replacement log"], cursor: 7000, replace: true, start: 6500, hasMoreBefore: true });
  assert.equal(day.viewer.children.length, 1);
  await day.older({ lines: ["previous log"], start: 6499, hasMoreBefore: true });
  assert.equal(new URL(day.requests.at(-1), "http://localhost").searchParams.get("before"), "6500",
    "a gap replacement must reset the pagination offset");

  const restarted = viewerContext(true);
  await restarted.poll({ generation: "generation-1", lines: ["before restart"], cursor: 11 });
  assert.equal(restarted.reloads, 0, "the same generation must not reload");
  await restarted.poll({ generation: "after-restart", lines: ["renumbered log"], cursor: 1 });
  assert.equal(restarted.reloads, 1, "a restarted index must reload the page");
  assert.equal(restarted.viewer.children.length, 1, "new-generation entries must not be appended");
  assert.equal(vm.runInContext("refreshCursor", restarted.context), 11, "a stale page must not adopt the new cursor");
  await restarted.pollStats({ generation: "after-restart" });
  assert.equal(restarted.reloads, 1, "overlapping polls must not trigger multiple reloads");

  const midnight = viewerContext(false);
  await midnight.pollStats({ generation: "generation-1" });
  assert.equal(midnight.reloads, 0);
  await midnight.pollStats({ generation: "after-midnight" });
  assert.equal(midnight.reloads, 1, "stats polling must detect midnight even without new logs");

  const older = viewerContext(true);
  await older.poll({ generation: "generation-1", lines: Array.from({ length: 5001 }, () => "log"), cursor: 5011 });
  await older.older({ generation: "after-midnight", lines: ["old generation"], start: 0 });
  assert.equal(older.reloads, 1, "pagination must also detect a generation change");
  assert.equal(older.viewer.children.length, 5000, "pagination must not apply a different generation");
}

main().catch(error => { console.error(error); process.exitCode = 1; });
