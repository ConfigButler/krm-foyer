// dist/sse.js
var SSEDecoder = class {
  #buffer = "";
  /** Feed a chunk of the stream; get back the events that completed with it. */
  push(chunk) {
    this.#buffer += chunk;
    const out = [];
    const trailingCR = this.#buffer.endsWith("\r");
    const complete = trailingCR ? this.#buffer.slice(0, -1) : this.#buffer;
    this.#buffer = complete.replace(/\r\n|\r/g, "\n") + (trailingCR ? "\r" : "");
    for (; ; ) {
      const sep = this.#buffer.indexOf("\n\n");
      if (sep === -1)
        break;
      const frame = this.#buffer.slice(0, sep);
      this.#buffer = this.#buffer.slice(sep + 2);
      const ev = parseFrame(frame);
      if (ev)
        out.push(ev);
    }
    return out;
  }
};
var StreamSequence = class {
  #next = 1;
  observe(event) {
    if (!Number.isSafeInteger(event.seq) || event.seq !== this.#next) {
      return { expected: this.#next, received: event.seq };
    }
    this.#next++;
    return null;
  }
};
function parseFrame(frame) {
  const data = [];
  for (const line of frame.split("\n")) {
    if (line === "" || line.startsWith(":"))
      continue;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" "))
      value = value.slice(1);
    if (field === "data")
      data.push(value);
  }
  if (data.length === 0)
    return null;
  try {
    return JSON.parse(data.join("\n"));
  } catch {
    return null;
  }
}
function applyStreamEvent(store, ev) {
  switch (ev.type) {
    case "reset":
      store.beginSnapshot();
      return { type: ev.type, added: false, structural: false, flashed: [], conflicts: [] };
    case "added":
    case "modified": {
      if (!ev.object)
        return { type: ev.type, added: false, structural: false, flashed: [], conflicts: [] };
      const result = store.applyServerEvent(ev.object, { redacted: ev.redacted });
      return { type: ev.type, uid: ev.object.metadata.uid, ...result };
    }
    case "deleted": {
      const uid = ev.identity?.uid;
      if (uid)
        store.removeResource(uid);
      return { type: ev.type, uid, added: false, structural: true, flashed: [], conflicts: [] };
    }
    case "synced":
      store.endSnapshot();
      return { type: ev.type, added: false, structural: false, flashed: [], conflicts: [] };
    default:
      return { type: ev.type, added: false, structural: false, flashed: [], conflicts: [] };
  }
}
function connectResourceStream(url, store, opts = {}) {
  if (opts.signal?.aborted)
    return { close: () => {
    }, closed: Promise.resolve() };
  const controller = new AbortController();
  const fetchImpl = opts.fetch ?? globalThis.fetch;
  const abort = () => controller.abort();
  opts.signal?.addEventListener("abort", abort, { once: true });
  const closed = (async () => {
    const res = await fetchImpl(url, {
      signal: controller.signal,
      headers: { Accept: "text/event-stream", ...opts.headers },
      // The stream IS the response body; a cached one is a stream that never moves.
      cache: "no-store",
      credentials: opts.credentials ?? "same-origin"
    });
    if (!res.ok || !res.body) {
      const code = res.status === 401 ? "UNAUTHENTICATED" : res.status === 403 ? "FORBIDDEN" : res.status === 429 || res.status === 502 || res.status === 503 || res.status === 504 ? "UPSTREAM_UNAVAILABLE" : "INTERNAL";
      const terminal = res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429;
      const message = await statusMessage(res, controller.signal) ?? `stream: HTTP ${res.status}`;
      if (controller.signal.aborted)
        return;
      opts.onError?.(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
      return;
    }
    if (controller.signal.aborted) {
      await res.body.cancel();
      return;
    }
    opts.onOpen?.();
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    const cancelReader = () => {
      void reader.cancel().catch(() => {
      });
    };
    controller.signal.addEventListener("abort", cancelReader, { once: true });
    if (controller.signal.aborted)
      cancelReader();
    const decoder = new SSEDecoder();
    const sequence = new StreamSequence();
    try {
      for (; ; ) {
        const { done, value } = await reader.read();
        if (done || controller.signal.aborted)
          return;
        for (const ev of decoder.push(value)) {
          if (controller.signal.aborted)
            return;
          if (feed(store, sequence, ev, opts)) {
            controller.abort();
            return;
          }
        }
      }
    } catch (err) {
      if (!controller.signal.aborted)
        throw err;
    } finally {
      controller.signal.removeEventListener("abort", cancelReader);
      await reader.cancel().catch(() => {
      });
    }
  })();
  return {
    close: () => controller.abort(),
    closed: closed.catch(() => {
    }).finally(() => opts.signal?.removeEventListener("abort", abort))
  };
}
var maxStatusBytes = 16 * 1024;
var statusBudgetMs = 2e3;
async function statusMessage(res, signal, budgetMs = statusBudgetMs) {
  if (!res.body)
    return void 0;
  if (signal.aborted || !/^application\/json\b/i.test(res.headers.get("Content-Type") ?? "")) {
    await res.body.cancel().catch(() => {
    });
    return void 0;
  }
  const reader = res.body.getReader();
  let gaveUp = false;
  const giveUp = () => {
    gaveUp = true;
    void reader.cancel().catch(() => {
    });
  };
  const timer = setTimeout(giveUp, budgetMs);
  signal.addEventListener("abort", giveUp, { once: true });
  const chunks = [];
  let size = 0;
  try {
    for (; ; ) {
      const { done, value } = await reader.read();
      if (gaveUp)
        return void 0;
      if (done)
        break;
      size += value.byteLength;
      if (size > maxStatusBytes)
        return void 0;
      chunks.push(value);
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.byteLength;
    }
    const status = JSON.parse(new TextDecoder().decode(bytes));
    if (typeof status !== "object" || status === null)
      return void 0;
    const { kind, message } = status;
    return kind === "Status" && typeof message === "string" && message !== "" ? message : void 0;
  } catch {
    return void 0;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener("abort", giveUp);
    await reader.cancel().catch(() => {
    });
  }
}
function retryAfter(header, now = Date.now()) {
  if (header === null)
    return void 0;
  const value = header.trim();
  if (/^\d+$/.test(value))
    return Number(value) * 1e3;
  const at2 = Date.parse(value);
  return Number.isNaN(at2) ? void 0 : Math.max(0, at2 - now);
}
function connectWithEventSource(url, store, opts = {}) {
  if (opts.signal?.aborted)
    return { close: () => {
    }, closed: Promise.resolve() };
  const es = new EventSource(url, { withCredentials: true });
  let sequence = new StreamSequence();
  let stopped = false;
  let resolve;
  const closed = new Promise((r) => {
    resolve = r;
  });
  const shut = () => {
    stopped = true;
    es.close();
    opts.signal?.removeEventListener("abort", shut);
    resolve();
  };
  es.onopen = () => {
    if (stopped)
      return;
    sequence = new StreamSequence();
    opts.onOpen?.();
  };
  es.onmessage = (e) => {
    if (stopped)
      return;
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch {
      return;
    }
    if (feed(store, sequence, ev, opts))
      shut();
  };
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED)
      shut();
  };
  opts.signal?.addEventListener("abort", shut, { once: true });
  return { close: shut, closed };
}
function feed(store, sequence, ev, opts) {
  const gap = sequence.observe(ev);
  if (gap) {
    opts.onGap?.(gap.expected, gap.received);
    return true;
  }
  if (ev.type === "error") {
    const hint = typeof ev.retryAfterMs === "number" && ev.retryAfterMs >= 0 ? ev.retryAfterMs : void 0;
    opts.onError?.(ev.code ?? "INTERNAL", ev.message ?? "", ev.terminal ?? false, hint);
    return ev.terminal === true;
  }
  const change = applyStreamEvent(store, ev);
  if (ev.type === "synced")
    opts.onSynced?.();
  opts.onChange?.(change);
  return false;
}

// dist/connection.js
function connectManagedResourceStream(url, store, opts = {}) {
  const maxRetries = opts.maxRetries ?? 8;
  const healthyResetMs = opts.healthyResetMs ?? 3e4;
  const delay = opts.retryDelayMs ?? 500;
  const cap = opts.maxRetryDelayMs ?? 3e4;
  if (!Number.isFinite(healthyResetMs) || healthyResetMs <= 0 || healthyResetMs > 2147483647 || !Number.isSafeInteger(maxRetries) || maxRetries < 0 || !Number.isFinite(delay) || delay < 0 || !Number.isFinite(cap) || cap < 0 || cap > 2147483647) {
    throw new RangeError("krm-stream: invalid retry budget or delay");
  }
  const controller = new AbortController();
  const subscribers = /* @__PURE__ */ new Set();
  let state = Object.freeze({ status: "connecting", retries: 0 });
  let terminal = false;
  let hintMs;
  let healthTimer;
  const clearHealthTimer = () => {
    clearTimeout(healthTimer);
    healthTimer = void 0;
  };
  const publish = (status, retryInMs) => {
    state = Object.freeze({ status, retries: state.retries, ...retryInMs === void 0 ? {} : { retryInMs } });
    opts.onStateChange?.(state);
    for (const callback of subscribers)
      callback(state);
  };
  const close = () => {
    clearHealthTimer();
    controller.abort();
  };
  opts.signal?.addEventListener("abort", close, { once: true });
  if (opts.signal?.aborted)
    close();
  const closed = Promise.resolve().then(async () => {
    try {
      while (!controller.signal.aborted) {
        publish("connecting");
        if (controller.signal.aborted)
          break;
        hintMs = void 0;
        const stream = connectResourceStream(url, store, {
          ...opts,
          signal: controller.signal,
          onGap: (expected, received) => {
            clearHealthTimer();
            opts.onGap?.(expected, received);
          },
          onOpen: () => {
            publish("syncing");
            opts.onOpen?.();
          },
          onChange: (change) => {
            if (change.type === "reset") {
              clearHealthTimer();
              publish("syncing");
            }
            opts.onChange?.(change);
          },
          onSynced: () => {
            hintMs = void 0;
            if (state.status !== "live") {
              healthTimer = setTimeout(() => {
                healthTimer = void 0;
                if (!controller.signal.aborted && state.status === "live") {
                  state = { ...state, retries: 0 };
                  publish("live");
                }
              }, healthyResetMs);
            }
            publish("live");
            opts.onSynced?.();
          },
          onError: (code, message, isTerminal, retryAfterMs) => {
            if (isTerminal)
              clearHealthTimer();
            terminal ||= isTerminal;
            if (!isTerminal && retryAfterMs !== void 0)
              hintMs = retryAfterMs;
            opts.onError?.(code, message, isTerminal, retryAfterMs);
          }
        });
        await stream.closed;
        clearHealthTimer();
        if (controller.signal.aborted)
          break;
        if (terminal) {
          publish("terminal");
          return;
        }
        if (state.retries >= maxRetries) {
          publish("exhausted");
          return;
        }
        const ceiling = Math.min(cap, delay * 2 ** Math.min(state.retries, 30));
        const jittered = Math.floor(ceiling * (0.5 + Math.random() * 0.5));
        const wait = Math.min(cap, Math.max(jittered, hintMs ?? 0));
        state = { ...state, retries: state.retries + 1 };
        publish("retrying", wait);
        await new Promise((resolve) => {
          const done = () => {
            clearTimeout(timer);
            controller.signal.removeEventListener("abort", done);
            resolve();
          };
          const timer = setTimeout(done, wait);
          controller.signal.addEventListener("abort", done, { once: true });
          if (controller.signal.aborted)
            done();
        });
      }
      publish("closed");
    } finally {
      clearHealthTimer();
      opts.signal?.removeEventListener("abort", close);
      subscribers.clear();
    }
  });
  return {
    close,
    closed,
    get state() {
      return state;
    },
    subscribe(callback) {
      subscribers.add(callback);
      return () => {
        subscribers.delete(callback);
      };
    }
  };
}

// dist/deep.js
function isPlainObject(v) {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
function deepEqual(a, b) {
  if (Object.is(a, b))
    return true;
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length)
      return false;
    return a.every((x, i) => deepEqual(x, b[i]));
  }
  if (isPlainObject(a) && isPlainObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    if (ka.length !== kb.length)
      return false;
    return ka.every((k) => Object.hasOwn(b, k) && deepEqual(a[k], b[k]));
  }
  return false;
}
function clone(v) {
  if (Array.isArray(v))
    return v.map(clone);
  if (isPlainObject(v)) {
    const out = {};
    for (const [k, x] of Object.entries(v))
      out[k] = clone(x);
    return out;
  }
  return v;
}

// dist/path.js
function pathKey(path) {
  return JSON.stringify(path);
}
function isPrefix(prefix, path) {
  if (prefix.length > path.length)
    return false;
  return prefix.every((seg, i) => String(seg) === String(path[i]));
}
function at(value, seg) {
  if (Array.isArray(value))
    return value[Number(seg)];
  if (isPlainObject(value))
    return value[String(seg)];
  return void 0;
}
function get(value, path) {
  let cur = value;
  for (const seg of path)
    cur = at(cur, seg);
  return cur;
}
function has(value, path) {
  let cur = value;
  for (const seg of path) {
    if (Array.isArray(cur)) {
      if (Number(seg) >= cur.length)
        return false;
    } else if (isPlainObject(cur)) {
      if (!Object.hasOwn(cur, String(seg)))
        return false;
    } else {
      return false;
    }
    cur = at(cur, seg);
  }
  return cur !== void 0;
}
function setAt(root, path, value) {
  if (path.length === 0)
    throw new Error("krm-stream: cannot set the root of an object");
  let cur = root;
  for (let i = 0; i < path.length - 1; i++) {
    const seg = path[i];
    let next = at(cur, seg);
    if (!isPlainObject(next) && !Array.isArray(next)) {
      next = typeof path[i + 1] === "number" ? [] : {};
      assign(cur, seg, next);
    }
    cur = next;
  }
  assign(cur, path[path.length - 1], value);
}
function removeAt(root, path) {
  if (path.length === 0)
    throw new Error("krm-stream: cannot remove the root of an object");
  const parent = get(root, path.slice(0, -1));
  const last = path[path.length - 1];
  if (Array.isArray(parent))
    parent.splice(Number(last), 1);
  else if (isPlainObject(parent))
    delete parent[String(last)];
}
function assign(container, seg, value) {
  if (Array.isArray(container))
    container[Number(seg)] = value;
  else
    container[String(seg)] = value;
}
function parsePointer(pointer) {
  if (pointer === "")
    return [];
  if (!pointer.startsWith("/"))
    throw new Error(`krm-stream: not an RFC 6901 pointer: ${JSON.stringify(pointer)}`);
  return pointer.slice(1).split("/").map((seg) => seg.replaceAll("~1", "/").replaceAll("~0", "~"));
}

// dist/policy.js
var DEFAULT_EDITABLE_REGIONS = [
  ["spec"],
  ["metadata", "labels"],
  ["metadata", "annotations"],
  ["data"],
  ["stringData"]
];
function regionPolicy(roots) {
  return {
    isEditable: (_obj, path) => roots.some((root) => isPrefix(root, path)),
    // A path that is not editable itself but that an editable region lives UNDER — `[]` and
    // `["metadata"]` for the defaults. The merge must recurse through these rather than replacing
    // them wholesale, or an edit to `metadata.labels` would be clobbered by the read-only handling
    // of `metadata.name` sitting beside it.
    containsEditable: (_obj, path) => roots.some((root) => root.length > path.length && isPrefix(path, root))
  };
}
var defaultPolicy = regionPolicy(DEFAULT_EDITABLE_REGIONS);
var readOnlyPolicy = regionPolicy([]);

// dist/schema.js
function withOpenAPIKeyedLists(policy, schema) {
  return {
    ...policy,
    listMapKeys: (object, path) => policy.listMapKeys?.(object, path) ?? mapKeysAt(schema, path)
  };
}
function mapKeysAt(root, path) {
  let schema = root;
  for (const segment of path) {
    schema = typeof segment === "number" ? schema?.items : schema?.properties?.[segment];
    if (!schema)
      return void 0;
  }
  const keys = schema["x-kubernetes-list-map-keys"];
  if (schema["x-kubernetes-list-type"] !== "map" || !keys || keys.length === 0)
    return void 0;
  return keys;
}

// dist/merge.js
function reconcile(s, base, ours, theirs) {
  return node(s, [], base, ours, theirs);
}
function node(s, path, base, ours, theirs) {
  if (s.regions.editable(path))
    return mergeEditable(s, path, base, ours, theirs);
  if (s.regions.container(path))
    return mergeContainer(s, path, base, ours, theirs);
  return follow(s, path, base, theirs);
}
function follow(s, path, base, theirs) {
  if (isPlainObject(base) && isPlainObject(theirs)) {
    const out = {};
    for (const k of unionKeys(base, theirs)) {
      const v = follow(s, [...path, k], at(base, k), at(theirs, k));
      if (v !== void 0)
        out[k] = v;
    }
    return out;
  }
  if (!deepEqual(base, theirs))
    s.flashed.push(path);
  return clone(theirs);
}
function mergeContainer(s, path, base, ours, theirs) {
  if (!isPlainObject(base) && !isPlainObject(ours) && !isPlainObject(theirs)) {
    return follow(s, path, base, theirs);
  }
  return recurse(s, path, base, ours, theirs);
}
function mergeEditable(s, path, base, ours, theirs) {
  const keys = s.regions.listMapKeys(path);
  if (keys && isAssociativeList(base, keys) && isAssociativeList(ours, keys) && isAssociativeList(theirs, keys)) {
    return mergeAssociativeList(s, path, keys, base, ours, theirs);
  }
  const defined = [base, ours, theirs].filter((v) => v !== void 0);
  if (defined.length > 0 && defined.every(isPlainObject))
    return recurse(s, path, base, ours, theirs);
  if (deepEqual(base, ours)) {
    if (!deepEqual(base, theirs))
      s.flashed.push(path);
    clearConflict(s, path);
    return clone(theirs);
  }
  if (deepEqual(base, theirs))
    return clone(ours);
  if (deepEqual(ours, theirs)) {
    clearConflict(s, path);
    return clone(ours);
  }
  setConflict(s, path, theirs);
  return clone(ours);
}
function mergeAssociativeList(s, path, keys, base, ours, theirs) {
  const baseByKey = indexAssociativeList(base, keys);
  const oursByKey = indexAssociativeList(ours, keys);
  const theirsByKey = indexAssociativeList(theirs, keys);
  if (!baseByKey || !oursByKey || !theirsByKey)
    return mergeAtomic(s, path, base, ours, theirs);
  const order = unique([...theirsByKey.order, ...oursByKey.order, ...baseByKey.order]);
  const outputKeys = order.filter((key) => associativeEntrySurvives(oursByKey.values.get(key), theirsByKey.values.get(key)));
  remapListConflicts(s, path, oursByKey.order, outputKeys);
  const out = [];
  for (const key of outputKeys) {
    const value = mergeAssociativeEntry(s, [...path, out.length], baseByKey.values.get(key), oursByKey.values.get(key), theirsByKey.values.get(key));
    if (value !== void 0)
      out.push(value);
  }
  return out;
}
function associativeEntrySurvives(ours, theirs) {
  return ours !== void 0 || theirs !== void 0;
}
function remapListConflicts(s, path, previousOrder, outputKeys) {
  const outputIndex = new Map(outputKeys.map((key, index) => [key, index]));
  const moved = [];
  for (const [encoded, conflict] of s.conflicts) {
    const segment = conflict.path[path.length];
    if (!isPrefix(path, conflict.path) || typeof segment !== "number")
      continue;
    const key = previousOrder[segment];
    const nextIndex = key === void 0 ? void 0 : outputIndex.get(key);
    if (nextIndex === void 0 || nextIndex === segment)
      continue;
    s.conflicts.delete(encoded);
    moved.push({ ...conflict, path: [...path, nextIndex, ...conflict.path.slice(path.length + 1)] });
  }
  for (const conflict of moved)
    s.conflicts.set(pathKey(conflict.path), conflict);
}
function mergeAssociativeEntry(s, path, base, ours, theirs) {
  if (base === void 0 || ours === void 0 || theirs === void 0)
    return mergeAtomic(s, path, base, ours, theirs);
  return node(s, path, base, ours, theirs);
}
function mergeAtomic(s, path, base, ours, theirs) {
  if (deepEqual(base, ours)) {
    if (!deepEqual(base, theirs))
      s.flashed.push(path);
    clearConflict(s, path);
    return clone(theirs);
  }
  if (deepEqual(base, theirs))
    return clone(ours);
  if (deepEqual(ours, theirs)) {
    clearConflict(s, path);
    return clone(ours);
  }
  setConflict(s, path, theirs);
  return clone(ours);
}
function isAssociativeList(value, keys) {
  return Array.isArray(value) && indexAssociativeList(value, keys) !== void 0;
}
function indexAssociativeList(values, keys) {
  const indexed = { order: [], values: /* @__PURE__ */ new Map() };
  for (const value of values) {
    if (!isPlainObject(value))
      return void 0;
    const identity = keys.map((key) => value[key]);
    if (identity.some((part) => part === void 0 || part === null || typeof part === "object"))
      return void 0;
    const encoded = JSON.stringify(identity);
    if (indexed.values.has(encoded))
      return void 0;
    indexed.order.push(encoded);
    indexed.values.set(encoded, value);
  }
  return indexed;
}
function unique(values) {
  return [...new Set(values)];
}
function recurse(s, path, base, ours, theirs) {
  const out = {};
  for (const k of unionKeys(base, ours, theirs)) {
    const v = node(s, [...path, k], at(base, k), at(ours, k), at(theirs, k));
    if (v !== void 0)
      out[k] = v;
  }
  if (theirs === void 0 && Object.keys(out).length === 0)
    return void 0;
  return out;
}
function unionKeys(...values) {
  const keys = [];
  const seen = /* @__PURE__ */ new Set();
  for (const v of values) {
    if (!isPlainObject(v))
      continue;
    for (const k of Object.keys(v)) {
      if (seen.has(k))
        continue;
      seen.add(k);
      keys.push(k);
    }
  }
  return keys;
}
function setConflict(s, path, theirs) {
  s.conflicts.set(pathKey(path), { path: [...path], theirs: clone(theirs) });
}
function clearConflict(s, path) {
  s.conflicts.delete(pathKey(path));
}

// dist/store.js
var LiveResourceStore = class {
  #policy;
  #revision = 0;
  #snapshotRevision = 0;
  #resources = /* @__PURE__ */ new Map();
  #subscribers = /* @__PURE__ */ new Set();
  /** Non-null exactly while a snapshot cycle is open: the uids seen since `beginSnapshot()`.
   * Pruning reads it in `endSnapshot()` and NOWHERE else — a cycle that never completes must prune
   * nothing, or a network hiccup makes the user watch half their resources evaporate (spec §5). */
  #seen = null;
  constructor(policy = defaultPolicy) {
    this.#policy = policy;
  }
  // ------------------------------------------------------------------ the stream in --
  /** `added` and `modified` — the only two upsert spellings, and they are treated identically
   * (spec §4). Both mean "here is this object's complete current state". */
  applyServerEvent(object, opts = {}) {
    const id = object.metadata.uid;
    const incoming = clone(object);
    const redacted = (opts.redacted ?? []).map((entry) => ({
      path: typeof entry.path === "string" ? parsePointer(entry.path) : [...entry.path],
      rev: entry.rev
    }));
    this.#seen?.add(id);
    const existing = this.#resources.get(id);
    if (!existing) {
      this.#resources.set(id, {
        revision: ++this.#revision,
        server: incoming,
        draft: clone(incoming),
        redacted,
        conflicts: /* @__PURE__ */ new Map()
      });
      this.#notify();
      return { added: true, structural: true, flashed: [], conflicts: [] };
    }
    const state = {
      regions: this.#regionsFor(incoming, redacted.map((r) => r.path)),
      conflicts: existing.conflicts,
      flashed: []
    };
    const merged = reconcile(state, existing.server, existing.draft, incoming);
    const structural = !sameShape(existing.draft, merged);
    existing.revision = ++this.#revision;
    existing.server = incoming;
    existing.draft = merged;
    existing.redacted = redacted;
    this.#notify();
    return {
      added: false,
      structural,
      flashed: state.flashed,
      conflicts: [...existing.conflicts.values()].map((c) => c.path)
    };
  }
  /** `deleted`. The object is gone, and so is any draft of it — the user was editing something that
   * no longer exists. (A recreate under the same name is a DIFFERENT uid and starts clean; that is
   * the whole reason identity is the uid.) */
  removeResource(id) {
    if (this.#resources.delete(id))
      this.#notify();
  }
  /** `reset`. Mark every known uid unseen — and prune NOTHING yet. */
  beginSnapshot() {
    this.#snapshotRevision++;
    this.#seen = /* @__PURE__ */ new Set();
  }
  /** `synced`. The snapshot is complete, so what it did not mention is genuinely gone. This is the
   * only place anything is pruned, and it is what removes an object deleted while the consumer was
   * disconnected — the one event the consumer never saw. */
  endSnapshot() {
    const seen = this.#seen;
    if (!seen)
      return;
    this.#seen = null;
    let pruned = false;
    for (const id of [...this.#resources.keys()]) {
      if (!seen.has(id)) {
        this.#resources.delete(id);
        pruned = true;
      }
    }
    if (pruned)
      this.#notify();
  }
  /** Adopt the object a save returned.
   *
   * `object` MUST be projected — the same projection the stream uses. An object straight from a
   * Kubernetes client carries managedFields, status and the Secret values the projection withholds,
   * and handing it here puts all of them in the browser through the one endpoint the stream does not
   * guard. Project it on the server with `gateway.Project` first.
   *
   * You probably do not need this. The recommended save is 204: the write reaches the API server, the
   * watch echoes it back down the stream already projected, and the store converges. Dirty state is
   * derived from draft-versus-server, so there is nothing to adopt. Use this only when a host cannot
   * wait for the echo. Unguarded adoption can overwrite newer watch state: for asynchronous
   * responses use captureReconciliation before the request instead. See docs/saving.md. */
  adoptSaved(object) {
    const existing = this.#resources.get(object.metadata.uid);
    if (!existing) {
      this.applyServerEvent(object);
      return;
    }
    this.applyServerEvent(object, { redacted: existing.redacted });
  }
  // ------------------------------------------------------------------------- edits --
  setValue(id, path, value) {
    const res = this.#editable(id, path);
    setAt(res.draft, path, clone(value));
    this.#settle(res, path);
  }
  /** For a map entry or an object key the user deleted. It stays deleted across watch events (the
   * merge sees ours=undefined, base=theirs and keeps the deletion) and becomes a `null` in the patch. */
  removeKey(id, path) {
    const res = this.#editable(id, path);
    removeAt(res.draft, path);
    this.#settle(res, path);
  }
  /** `path` addresses the MAP; `key` is the new entry. A new row in a UI starts empty. */
  addKey(id, path, key, value = "") {
    const res = this.#editable(id, [...path, key]);
    setAt(res.draft, [...path, key], clone(value));
    this.#settle(res, [...path, key]);
  }
  /** `path` addresses the MAP. Order is preserved — renaming a label must not make its row jump to
   * the bottom of the list while the user is typing in it. */
  renameKey(id, path, oldKey, newKey) {
    const res = this.#editable(id, [...path, oldKey]);
    this.#editable(id, [...path, newKey]);
    const map = get(res.draft, path);
    if (!isPlainObject(map))
      throw new Error(`krm-stream: ${pathKey(path)} is not a map`);
    if (!Object.hasOwn(map, oldKey))
      throw new Error(`krm-stream: key ${JSON.stringify(oldKey)} does not exist`);
    if (oldKey !== newKey && Object.hasOwn(map, newKey)) {
      throw new Error(`krm-stream: key ${JSON.stringify(newKey)} already exists`);
    }
    const renamed = {};
    for (const [k, v] of Object.entries(map))
      renamed[k === oldKey ? newKey : k] = v;
    setAt(res.draft, path, renamed);
    this.#settle(res, [...path, oldKey]);
    this.#settle(res, [...path, newKey]);
  }
  /** Throw the local edit away and take the server's value — which is also how a conflict is
   * resolved in the server's favour. */
  revert(id, path) {
    const res = this.#editable(id, path);
    if (has(res.server, path))
      setAt(res.draft, path, clone(get(res.server, path)));
    else
      removeAt(res.draft, path);
    this.#settle(res, path);
  }
  /** Resolve a conflict by keeping the server's value. */
  takeTheirs(id, path) {
    this.revert(id, path);
  }
  // ----------------------------------------------------------------------- queries --
  ids() {
    return [...this.#resources.keys()];
  }
  /** The authoritative server object, including live `status`. */
  server(id) {
    return clone(this.#must(id).server);
  }
  /** The server object with the editable regions merged over it. What a UI renders and edits. */
  draft(id) {
    return clone(this.#must(id).draft);
  }
  /** Read-only convenience for the watch UI. A kind with no status simply has none. */
  status(id) {
    return clone(this.#must(id).server.status);
  }
  /** Every pending edit, derived fresh by comparing the draft to the server object. Arrays appear
   * whole (§4.1), which is also what RFC 7386 wants. */
  changes(id) {
    const res = this.#must(id);
    const out = [];
    this.#diff(this.#regionsFor(res.server, res.redacted.map((r) => r.path)), [], res.server, res.draft, out);
    return out;
  }
  /** R-DERIVED: never a stored flag. A read-only path is never dirty by construction. */
  isDirty(id, path) {
    const res = this.#must(id);
    if (this.isEditable(id, path))
      return !deepEqual(get(res.server, path), get(res.draft, path));
    if (!this.#policy.containsEditable(res.server, path))
      return false;
    return this.changes(id).some((c) => isPrefix(path, c.path));
  }
  conflicts(id) {
    return [...this.#must(id).conflicts.values()].map((c) => ({ path: [...c.path], theirs: clone(c.theirs) }));
  }
  /** The policy, minus this object's redacted paths. A redacted value is read-only for exactly the
   * same reason `status` is: it is not the user's to change — and here, they never even saw it. */
  isEditable(id, path) {
    const res = this.#must(id);
    return this.#regionsFor(res.server, res.redacted.map((r) => r.path)).editable(path);
  }
  /**
   * The paths whose values the gateway withheld. **This is the only place a consumer learns that a
   * redacted field exists**, and rendering it is the whole of keys-only disclosure.
   *
   * The value itself is not in the object — there is no mask, no placeholder, nothing (spec §3). So a
   * UI showing `token ••••••` reads it from HERE, not from the object:
   *
   * ```ts
   * for (const { path } of store.redactions(uid)) row(path, "••••••", { readOnly: true });
   * ```
   *
   * That is deliberate. A placeholder sitting in the object is a value a browser can save back — and
   * a merge patch carrying it writes the placeholder over the real Secret.
   */
  redactions(id) {
    return this.#must(id).redacted.map((r) => ({ path: [...r.path], rev: r.rev }));
  }
  /** An RFC 7386 merge patch of the editable changes, or null when there is nothing to save.
   *
   * Built from the user's EDITS — never from a diff of the whole object against the server. The
   * object on the wire is a projection: a path the projection removed is simply not there, and a
   * whole-object diff would read that absence as a deletion and try to patch it away (spec §3). */
  patch(id) {
    const changes = this.changes(id);
    if (changes.length === 0)
      return null;
    const out = {};
    for (const c of changes)
      setAt(out, c.path, c.new === void 0 ? null : clone(c.new));
    return out;
  }
  /** Capture the patch, UID and merge-base version synchronously, before any await. A stale version
   * is safe: Kubernetes rejects it with 409. Never replace it with a newer GET's version. */
  captureSave(id) {
    const resource = this.#must(id);
    const patch = this.patch(id);
    if (!patch)
      return null;
    const resourceVersion = resource.server.metadata.resourceVersion;
    if (!resourceVersion)
      throw new Error("krm-stream: conditional save requires resourceVersion");
    return { uid: resource.server.metadata.uid, resourceVersion, patch };
  }
  /** Capture before starting a host GET. The returned function applies its projected response only
   * if no server event or earlier response has advanced this resource since capture. Local edits
   * remain valid and are three-way merged. Missing/recreated resources cannot be resurrected.
   * Snapshot recovery also invalidates responses; a GET must never count as snapshot membership.
   * Use the same projection as the stream. Omitted metadata preserves existing redactions.
   * redactedPaths preserves known revisions and removes absent paths; an unknown path rejects the
   * response. A later authoritative upsert or fresh snapshot can supply the missing revision counters. */
  captureReconciliation(id) {
    const revision = this.#must(id).revision;
    const snapshotRevision = this.#snapshotRevision;
    return (object, opts = {}) => {
      if (this.#seen !== null || this.#snapshotRevision !== snapshotRevision || object.metadata.uid !== id || this.#resources.get(id)?.revision !== revision)
        return false;
      const existing = this.#must(id).redacted;
      let redacted = opts.redacted ?? existing;
      if (opts.redactedPaths !== void 0) {
        const known = new Map(existing.map((entry) => [pathKey(entry.path), entry]));
        const paths = opts.redactedPaths.map(parsePointer);
        if (paths.some((path) => !known.has(pathKey(path))))
          return false;
        redacted = paths.map((path) => known.get(pathKey(path)));
      }
      this.applyServerEvent(object, { redacted });
      return true;
    };
  }
  /** A coarse "something changed" signal — the host re-renders and re-queries. Returns an
   * unsubscribe. */
  subscribe(cb) {
    this.#subscribers.add(cb);
    return () => this.#subscribers.delete(cb);
  }
  // ---------------------------------------------------------------------- internals --
  #must(id) {
    const res = this.#resources.get(id);
    if (!res)
      throw new Error(`krm-stream: no resource ${JSON.stringify(id)}`);
    return res;
  }
  /** Every edit goes through here: a write to `status`, to `metadata.name`, or to a redacted path is
   * refused. The engine is not the security boundary — the gateway rejects such a patch too — but a
   * UI that cannot even form the edit is safer than one that relies on client cooperation. */
  #editable(id, path) {
    const res = this.#must(id);
    if (!this.isEditable(id, path)) {
      throw new Error(`krm-stream: ${pathKey(path)} is read-only`);
    }
    return res;
  }
  #regionsFor(object, redacted) {
    const insideRedacted = (path) => redacted.some((r) => isPrefix(r, path));
    const containsRedacted = (path) => redacted.some((r) => isPrefix(path, r));
    return {
      editable: (path) => !insideRedacted(path) && !containsRedacted(path) && this.#policy.isEditable(object, path),
      container: (path) => !insideRedacted(path) && this.#policy.containsEditable(object, path),
      listMapKeys: (path) => this.#policy.listMapKeys?.(object, path)
    };
  }
  /** After an edit: a conflict whose path the draft has now brought back to the server's value is no
   * longer a conflict. (Typing the server's value by hand resolves it, and so does `revert`.) */
  #settle(res, path) {
    for (const [k, c] of res.conflicts) {
      if (!isPrefix(c.path, path) && !isPrefix(path, c.path))
        continue;
      if (deepEqual(get(res.server, c.path), get(res.draft, c.path)))
        res.conflicts.delete(k);
    }
    this.#notify();
  }
  #diff(regions, path, srv, drf, out) {
    if (regions.editable(path)) {
      const defined = [srv, drf].filter((v) => v !== void 0);
      if (defined.length > 0 && defined.every(isPlainObject)) {
        for (const k of unionKeys2(srv, drf)) {
          this.#diff(regions, [...path, k], srv?.[k], drf?.[k], out);
        }
        return;
      }
      if (!deepEqual(srv, drf)) {
        const kind = srv === void 0 ? "add" : drf === void 0 ? "delete" : "update";
        out.push({ path: [...path], kind, old: clone(srv), new: clone(drf) });
      }
      return;
    }
    if (regions.container(path)) {
      for (const k of unionKeys2(srv, drf)) {
        this.#diff(regions, [...path, k], srv?.[k], drf?.[k], out);
      }
    }
  }
  #notify() {
    for (const cb of this.#subscribers)
      cb();
  }
};
function unionKeys2(...values) {
  const keys = [];
  const seen = /* @__PURE__ */ new Set();
  for (const v of values) {
    if (!isPlainObject(v))
      continue;
    for (const k of Object.keys(v)) {
      if (!seen.has(k)) {
        seen.add(k);
        keys.push(k);
      }
    }
  }
  return keys;
}
function sameShape(a, b) {
  if (isPlainObject(a) && isPlainObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    if (ka.length !== kb.length)
      return false;
    return ka.every((k) => Object.hasOwn(b, k) && sameShape(a[k], b[k]));
  }
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((x, i) => sameShape(x, b[i]));
  }
  return isPlainObject(a) === isPlainObject(b) && Array.isArray(a) === Array.isArray(b);
}

// dist/url.js
function resourceStreamURL(base, scope) {
  if (!scope.resource)
    throw new Error("krm-stream: scope needs a `resource` (the plural, lowercase API name)");
  if (!scope.version)
    throw new Error("krm-stream: scope needs a `version` (there is no default: v1 and v1beta1 differ)");
  const q = new URLSearchParams();
  const set = (k, v) => {
    if (v)
      q.append(k, v);
  };
  set("target", scope.target);
  set("group", scope.group);
  set("version", scope.version);
  set("resource", scope.resource);
  set("namespace", scope.namespace);
  set("name", scope.name);
  set("labelSelector", scope.labelSelector);
  set("projection", scope.projection);
  return `${base}${base.includes("?") ? "&" : "?"}${q.toString()}`;
}

// dist/version.js
var VERSION = "0.5.0";
var PROTOCOL_VERSION = 1;
export {
  DEFAULT_EDITABLE_REGIONS,
  LiveResourceStore,
  PROTOCOL_VERSION,
  SSEDecoder,
  StreamSequence,
  VERSION,
  applyStreamEvent,
  clone,
  connectManagedResourceStream,
  connectResourceStream,
  connectWithEventSource,
  deepEqual,
  defaultPolicy,
  get,
  has,
  isPrefix,
  parsePointer,
  pathKey,
  readOnlyPolicy,
  regionPolicy,
  resourceStreamURL,
  withOpenAPIKeyedLists
};
