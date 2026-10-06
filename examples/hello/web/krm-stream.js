// dist/lifecycle.js
function runConnection(transport, consume, opts) {
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
  let failure;
  const fail = (error) => {
    failure ??= { error };
    clearHealthTimer();
    controller.abort();
  };
  const call = (callback) => {
    try {
      callback();
    } catch (error) {
      fail(error);
    }
  };
  const publish = (status, detail = {}) => {
    state = Object.freeze({ status, retries: state.retries, ...detail });
    for (const callback of subscribers)
      call(() => callback(state));
  };
  const close = () => {
    clearHealthTimer();
    controller.abort();
  };
  opts.signal?.addEventListener("abort", close, { once: true });
  if (opts.signal?.aborted)
    close();
  const run = async () => {
    while (!controller.signal.aborted) {
      publish("connecting");
      if (controller.signal.aborted)
        break;
      hintMs = void 0;
      let gap;
      await transport({
        consume: (event) => call(() => consume(event)),
        opened: () => publish("syncing"),
        reset: () => {
          clearHealthTimer();
          publish("syncing");
        },
        synced: () => {
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
        },
        gap: (expected, received) => {
          clearHealthTimer();
          gap = Object.freeze({ expected, received });
        },
        error: (code, message, isTerminal, retryAfterMs) => {
          if (isTerminal)
            clearHealthTimer();
          terminal ||= isTerminal;
          if (!isTerminal && retryAfterMs !== void 0)
            hintMs = retryAfterMs;
          call(() => opts.onError?.(code, message, isTerminal, retryAfterMs));
        }
      }, controller.signal);
      clearHealthTimer();
      if (controller.signal.aborted)
        break;
      if (terminal)
        return "terminal";
      if (state.retries >= maxRetries)
        return "exhausted";
      const ceiling = Math.min(cap, delay * 2 ** Math.min(state.retries, 30));
      const jittered = Math.floor(ceiling * (0.5 + Math.random() * 0.5));
      const wait = Math.min(cap, Math.max(jittered, hintMs ?? 0));
      state = { ...state, retries: state.retries + 1 };
      publish("retrying", { retryInMs: wait, ...gap && { gap } });
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
    return "closed";
  };
  const closed = Promise.resolve().then(async () => {
    try {
      const ended = await run();
      publish(failure ? "closed" : ended);
    } finally {
      clearHealthTimer();
      opts.signal?.removeEventListener("abort", close);
      subscribers.clear();
    }
    if (failure)
      throw failure.error;
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

// dist/http.js
function request(url, accept, signal, opts) {
  return (opts.fetch ?? globalThis.fetch)(url, {
    signal,
    headers: { Accept: accept, ...opts.headers },
    cache: "no-store",
    credentials: opts.credentials ?? "same-origin"
  });
}
function refusal(status) {
  const code = status === 401 ? "UNAUTHENTICATED" : status === 403 ? "FORBIDDEN" : status === 429 || status === 502 || status === 503 || status === 504 ? "UPSTREAM_UNAVAILABLE" : "INTERNAL";
  return { code, terminal: status >= 400 && status < 500 && status !== 408 && status !== 429 };
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

// dist/version.js
var VERSION = "0.10.0";
var PROTOCOL_VERSION = 1;

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
function toStateEvent(wire) {
  switch (wire.type) {
    case "reset":
      return {
        type: "reset",
        ...wire.target === void 0 ? {} : { target: wire.target },
        ...wire.scope === void 0 ? {} : { scope: wire.scope },
        ...wire.projection === void 0 ? {} : { projection: wire.projection }
      };
    case "added":
    case "modified":
      if (!wire.object)
        return null;
      return {
        type: wire.type,
        object: wire.object,
        ...wire.redacted === void 0 ? {} : { redacted: wire.redacted }
      };
    case "deleted":
      if (!wire.identity?.uid)
        return null;
      return { type: "deleted", identity: wire.identity };
    case "synced":
      return { type: "synced" };
    default:
      return null;
  }
}
async function streamOnce(url, hooks, signal, opts = {}) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted)
    abort();
  const sequence = new StreamSequence();
  const { consume } = hooks;
  const deliver2 = (wire) => {
    const gap = sequence.observe(wire);
    if (gap) {
      hooks.gap(gap.expected, gap.received);
      return false;
    }
    if (wire.type === "error") {
      const hint = typeof wire.retryAfterMs === "number" && wire.retryAfterMs >= 0 ? wire.retryAfterMs : void 0;
      hooks.error(wire.code ?? "INTERNAL", wire.message ?? "", wire.terminal ?? false, hint);
      return wire.terminal !== true;
    }
    const event = toStateEvent(wire);
    if (!event)
      return true;
    if (event.type === "reset") {
      hooks.reset();
      if (controller.signal.aborted)
        return false;
    }
    consume(event);
    if (event.type === "synced" && !controller.signal.aborted)
      hooks.synced();
    return true;
  };
  try {
    if (controller.signal.aborted)
      return;
    const res = await request(url, "text/event-stream", controller.signal, opts);
    if (!res.ok || !res.body) {
      const { code, terminal } = refusal(res.status);
      const message = await statusMessage(res, controller.signal) ?? `stream: HTTP ${res.status}`;
      if (controller.signal.aborted)
        return;
      hooks.error(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
      return;
    }
    if (controller.signal.aborted) {
      await res.body.cancel().catch(() => {
      });
      return;
    }
    const protocol = res.headers.get("X-KRM-Stream-Protocol");
    if (protocol !== null && protocol.trim() !== String(PROTOCOL_VERSION)) {
      await res.body.cancel().catch(() => {
      });
      if (controller.signal.aborted)
        return;
      hooks.error("INTERNAL", `stream: protocol mismatch: the gateway speaks X-KRM-Stream-Protocol ${JSON.stringify(protocol)}, this client speaks ${PROTOCOL_VERSION}`, true);
      return;
    }
    hooks.opened();
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    const cancelReader = () => {
      void reader.cancel().catch(() => {
      });
    };
    controller.signal.addEventListener("abort", cancelReader, { once: true });
    if (controller.signal.aborted)
      cancelReader();
    const decoder = new SSEDecoder();
    try {
      for (; ; ) {
        const { done, value } = await reader.read();
        if (done || controller.signal.aborted)
          return;
        for (const ev of decoder.push(value)) {
          if (controller.signal.aborted)
            return;
          if (!deliver2(ev)) {
            controller.abort();
            return;
          }
        }
      }
    } finally {
      controller.signal.removeEventListener("abort", cancelReader);
      await reader.cancel().catch(() => {
      });
    }
  } catch {
  } finally {
    signal.removeEventListener("abort", abort);
  }
}

// dist/connection.js
function connectResourceStream(url, consume, opts = {}) {
  return runConnection((hooks, signal) => streamOnce(url, hooks, signal, opts), consume, opts);
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

// dist/native.js
var reserved = [
  "watch",
  "resourceVersion",
  "resourceVersionMatch",
  "limit",
  "continue",
  "sendInitialEvents",
  "allowWatchBookmarks"
];
function connectNativeWatch(collectionURL, consume, opts = {}) {
  const url = collectionURL.split("#")[0];
  const query = new URLSearchParams(url.includes("?") ? url.slice(url.indexOf("?") + 1) : "");
  for (const name of reserved) {
    if (query.has(name))
      throw new Error(`krm-stream: the collection URL must not set ${name}; the connector does`);
  }
  const position = { checkpoint: void 0, type: void 0 };
  return runConnection((hooks, signal) => watchOnce(url, hooks, signal, opts, position), consume, opts);
}
var Malformed = class extends Error {
};
var Truncated = class extends Error {
};
var resumable = (status) => status === 408 || status === 429 || status >= 500;
async function watchOnce(url, hooks, signal, opts, position) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted)
    abort();
  const aborted = () => controller.signal.aborted;
  const { consume } = hooks;
  const refused = async (phase, res) => {
    if (!resumable(res.status))
      position.checkpoint = void 0;
    const { code, terminal } = classify(res.status);
    const message = await statusMessage(res, controller.signal) ?? `native ${phase}: HTTP ${res.status}`;
    if (aborted())
      return;
    hooks.error(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
  };
  const malformed = (phase, error) => {
    position.checkpoint = void 0;
    if (!aborted())
      hooks.error("INTERNAL", `native ${phase}: ${error.message}`, false);
  };
  const advance = (resourceVersion) => {
    if (aborted())
      return;
    position.checkpoint = typeof resourceVersion === "string" && resourceVersion !== "" ? resourceVersion : void 0;
  };
  try {
    if (aborted())
      return;
    const resumeFrom = position.checkpoint;
    let from;
    if (resumeFrom === void 0) {
      const listed = await request(url, "application/json", controller.signal, opts);
      if (!listed.ok || !listed.body)
        return await refused("list", listed);
      if (aborted())
        return void await listed.body.cancel().catch(() => {
        });
      hooks.opened();
      let collection;
      try {
        const text = await readText(listed.body, controller.signal);
        if (text === void 0)
          return;
        collection = readCollection(text);
      } catch (error) {
        if (error instanceof Unpaginated) {
          if (!aborted())
            hooks.error("INTERNAL", error.message, true);
          return;
        }
        if (error instanceof Malformed)
          return malformed("list", error);
        throw error;
      }
      position.type = collection.type;
      from = collection.resourceVersion;
      hooks.reset();
      if (aborted())
        return;
      consume({ type: "reset" });
      for (const object of collection.items) {
        if (aborted())
          return;
        consume({ type: "added", object });
      }
      if (aborted())
        return;
    } else {
      from = resumeFrom;
    }
    const watched = await request(`${url}${url.includes("?") ? "&" : "?"}watch=1&allowWatchBookmarks=true&resourceVersion=${encodeURIComponent(from)}`, "application/json", controller.signal, opts);
    if (!watched.ok || !watched.body)
      return await refused("watch", watched);
    if (aborted())
      return void await watched.body.cancel().catch(() => {
      });
    if (resumeFrom === void 0) {
      consume({ type: "synced" });
      if (aborted())
        return void await watched.body.cancel().catch(() => {
        });
      position.checkpoint = from;
    }
    hooks.synced();
    const reader = watched.body.getReader();
    const cancelReader = () => {
      void reader.cancel().catch(() => {
      });
    };
    controller.signal.addEventListener("abort", cancelReader, { once: true });
    if (aborted())
      cancelReader();
    const decoder = new WatchDecoder();
    try {
      for (; ; ) {
        const { done, value } = await reader.read();
        if (aborted())
          return;
        let lines;
        try {
          if (done)
            return decoder.end();
          lines = decoder.push(value);
        } catch (error) {
          if (error instanceof Truncated) {
            return void hooks.error("INTERNAL", `native watch: ${error.message}`, false);
          }
          if (!(error instanceof Malformed))
            throw error;
          return malformed("watch", error);
        }
        for (const line of lines) {
          if (aborted())
            return;
          let more;
          try {
            more = deliver(line, position.type, hooks, advance, position);
          } catch (error) {
            if (!(error instanceof Malformed))
              throw error;
            return malformed("watch", error);
          }
          if (!more)
            return;
        }
      }
    } finally {
      controller.signal.removeEventListener("abort", cancelReader);
      await reader.cancel().catch(() => {
      });
    }
  } catch {
  } finally {
    controller.abort();
    signal.removeEventListener("abort", abort);
  }
}
function deliver(line, type, hooks, advance, position) {
  let frame;
  try {
    frame = JSON.parse(line);
  } catch {
    throw new Malformed("a watch frame is not JSON");
  }
  if (!isRecord(frame) || typeof frame.type !== "string" || !isRecord(frame.object)) {
    throw new Malformed("a watch frame has no type or object");
  }
  const object = frame.object;
  switch (frame.type) {
    case "ADDED":
    case "MODIFIED": {
      const applied = resource(object, type, "watch");
      hooks.consume({ type: frame.type === "ADDED" ? "added" : "modified", object: applied });
      advance(applied.metadata.resourceVersion);
      return true;
    }
    case "DELETED": {
      const removed = resource(object, type, "watch");
      hooks.consume({ type: "deleted", identity: identity(removed) });
      advance(removed.metadata.resourceVersion);
      return true;
    }
    case "BOOKMARK":
      advance(isRecord(object.metadata) ? object.metadata.resourceVersion : void 0);
      return true;
    case "ERROR": {
      const { code, terminal, message, retryAfterMs, keepsCheckpoint } = statusError(object);
      if (!keepsCheckpoint)
        position.checkpoint = void 0;
      hooks.error(code, message, terminal, retryAfterMs);
      return false;
    }
    default:
      throw new Malformed(`unknown watch event type ${JSON.stringify(frame.type)}`);
  }
}
function classify(status) {
  return status === 410 ? { code: "RESYNC_REQUIRED", terminal: false } : refusal(status);
}
function statusError(status) {
  const reason = typeof status.reason === "string" ? status.reason : void 0;
  const expired = reason === "Expired" || reason === "Gone";
  const code = typeof status.code === "number" ? status.code : expired ? 410 : void 0;
  const { code: errorCode, terminal } = code === void 0 ? { code: "INTERNAL", terminal: false } : classify(code);
  const text = typeof status.message === "string" ? status.message : "";
  const details = status.details;
  const seconds = isRecord(details) ? details.retryAfterSeconds : void 0;
  return {
    code: errorCode,
    terminal,
    message: text !== "" ? text : `native watch: error ${code ?? reason ?? "without a status"}`,
    retryAfterMs: typeof seconds === "number" && seconds >= 0 ? seconds * 1e3 : void 0,
    keepsCheckpoint: code !== void 0 && !expired && resumable(code)
  };
}
var Unpaginated = class extends Error {
};
function readCollection(text) {
  let body;
  try {
    body = JSON.parse(text);
  } catch {
    throw new Malformed("the collection is not JSON");
  }
  if (!isRecord(body))
    throw new Malformed("the collection is not an object");
  const metadata = isRecord(body.metadata) ? body.metadata : {};
  if (typeof metadata.continue === "string" && metadata.continue !== "") {
    throw new Unpaginated("native list: the response is one page of a paginated collection, which this connector does not support");
  }
  const resourceVersion = metadata.resourceVersion;
  if (typeof resourceVersion !== "string" || resourceVersion === "") {
    throw new Malformed("the collection has no metadata.resourceVersion");
  }
  const raw = body.items;
  if (raw !== null && !Array.isArray(raw))
    throw new Malformed("the collection has no items array");
  const type = itemType(body);
  const uids = /* @__PURE__ */ new Set();
  const items = (raw ?? []).map((item) => {
    const object = resource(item, type, "list");
    if (uids.has(object.metadata.uid))
      throw new Malformed(`the collection lists UID ${object.metadata.uid} twice`);
    uids.add(object.metadata.uid);
    return object;
  });
  return { items, resourceVersion, type };
}
function itemType(collection) {
  const { apiVersion, kind } = collection;
  if (typeof apiVersion !== "string" || apiVersion === "" || apiVersion.startsWith("meta.k8s.io/"))
    return void 0;
  if (typeof kind !== "string" || !/^[A-Za-z0-9]+List$/.test(kind))
    return void 0;
  return { apiVersion, kind: kind.slice(0, -"List".length) };
}
function resource(value, type, phase) {
  const where = phase === "list" ? "a collection item" : "a watch object";
  if (!isRecord(value) || !isRecord(value.metadata))
    throw new Malformed(`${where} has no metadata`);
  const { uid, name, namespace } = value.metadata;
  if (typeof uid !== "string" || uid === "")
    throw new Malformed(`${where} has no metadata.uid`);
  if (typeof name !== "string" || name === "")
    throw new Malformed(`${where} ${uid} has no metadata.name`);
  if (namespace !== void 0 && typeof namespace !== "string") {
    throw new Malformed(`${where} ${uid} has an invalid metadata.namespace`);
  }
  const present = (field) => typeof field === "string" && field !== "";
  if (present(value.apiVersion) && present(value.kind))
    return value;
  const apiVersion = present(value.apiVersion) ? value.apiVersion : type?.apiVersion;
  const kind = present(value.kind) ? value.kind : type?.kind;
  if (apiVersion === void 0 || kind === void 0) {
    throw new Malformed(`${where} ${uid} has no apiVersion or kind, and the collection does not name its item type`);
  }
  return { ...value, apiVersion, kind };
}
function identity(object) {
  const { uid, name, namespace } = object.metadata;
  return {
    uid,
    apiVersion: object.apiVersion,
    kind: object.kind,
    ...namespace === void 0 ? {} : { namespace },
    name
  };
}
var WatchDecoder = class {
  #utf8 = new TextDecoder("utf-8", { fatal: true });
  #buffer = "";
  /** Feed a chunk; get back the complete, nonblank lines it finished. */
  push(chunk) {
    this.#buffer += decode(this.#utf8, chunk);
    const lines = [];
    for (; ; ) {
      const end = this.#buffer.indexOf("\n");
      if (end === -1)
        break;
      const line = this.#buffer.slice(0, end).trim();
      this.#buffer = this.#buffer.slice(end + 1);
      if (line !== "")
        lines.push(line);
    }
    return lines;
  }
  /** The stream ended. Anything still buffered — a character cut off included — is a truncated frame:
   * every complete line has already been returned, so the cut can only be inside the last one. */
  end() {
    let rest;
    try {
      rest = this.#utf8.decode();
    } catch {
      throw new Truncated("the watch ended inside a UTF-8 character");
    }
    if ((this.#buffer + rest).trim() !== "")
      throw new Truncated("the watch ended inside a frame");
  }
};
async function readText(body, signal) {
  const reader = body.getReader();
  const cancel = () => {
    void reader.cancel().catch(() => {
    });
  };
  signal.addEventListener("abort", cancel, { once: true });
  if (signal.aborted)
    cancel();
  const utf8 = new TextDecoder("utf-8", { fatal: true });
  let text = "";
  try {
    for (; ; ) {
      const { done, value } = await reader.read();
      if (signal.aborted)
        return void 0;
      if (done)
        return text + decode(utf8);
      text += decode(utf8, value);
    }
  } finally {
    signal.removeEventListener("abort", cancel);
    await reader.cancel().catch(() => {
    });
  }
}
function decode(utf8, chunk) {
  try {
    return chunk === void 0 ? utf8.decode() : utf8.decode(chunk, { stream: true });
  } catch {
    throw new Malformed("the response is not valid UTF-8");
  }
}
function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
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
  for (const segment2 of path) {
    schema = typeof segment2 === "number" ? schema?.items : schema?.properties?.[segment2];
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
    const segment2 = conflict.path[path.length];
    if (!isPrefix(path, conflict.path) || typeof segment2 !== "number")
      continue;
    const key = previousOrder[segment2];
    const nextIndex = key === void 0 ? void 0 : outputIndex.get(key);
    if (nextIndex === void 0 || nextIndex === segment2)
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
    const identity2 = keys.map((key) => value[key]);
    if (identity2.some((part) => part === void 0 || part === null || typeof part === "object"))
      return void 0;
    const encoded = JSON.stringify(identity2);
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
function applyStreamEvent(store, event) {
  switch (event.type) {
    case "reset":
      store.beginSnapshot();
      return { type: event.type, added: false, structural: false, flashed: [], conflicts: [] };
    case "added":
    case "modified": {
      const result = store.applyServerEvent(event.object, { redacted: event.redacted });
      return { type: event.type, uid: event.object.metadata.uid, ...result };
    }
    case "deleted": {
      const uid = event.identity.uid;
      store.removeResource(uid);
      return { type: event.type, uid, added: false, structural: true, flashed: [], conflicts: [] };
    }
    case "synced":
      store.endSnapshot();
      return { type: event.type, added: false, structural: false, flashed: [], conflicts: [] };
  }
}
var MACHINERY = [
  ["metadata", "managedFields"],
  ["metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"]
];
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
   * (spec §4). Both mean "here is this object's complete current state", and during a snapshot cycle
   * that the object is still in scope. */
  applyServerEvent(object, opts = {}) {
    this.#seen?.add(object.metadata.uid);
    return this.#upsert(object, opts);
  }
  /** Replace the server object and reconcile the draft. Membership is the caller's business: only the
   * stream's own upserts count towards a snapshot, so a save or read response that lands mid-cycle
   * can never keep alive an object the snapshot no longer contains. */
  #upsert(object, opts) {
    const id = object.metadata.uid;
    const incoming = clone(object);
    const redacted = (opts.redacted ?? []).map((entry) => ({
      path: typeof entry.path === "string" ? parsePointer(entry.path) : [...entry.path],
      rev: entry.rev
    }));
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
      this.#upsert(object, {});
      return;
    }
    this.#upsert(object, { redacted: existing.redacted });
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
    const regions = this.#regionsFor(res.server, res.redacted.map((r) => r.path));
    if (!regions.container(path))
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
    const resource2 = this.#must(id);
    const patch = this.patch(id);
    if (!patch)
      return null;
    const resourceVersion = resource2.server.metadata.resourceVersion;
    if (!resourceVersion)
      throw new Error("krm-stream: conditional save requires resourceVersion");
    return { uid: resource2.server.metadata.uid, resourceVersion, patch };
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
      this.#upsert(object, { redacted });
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
  /** The policy, minus the protected paths: this object's redactions and the machinery. A path
   * inside one is read-only. A path holding one cannot be replaced or removed whole, which would
   * rewrite what it holds, so where the policy makes it editable it is merged key by key instead: a
   * new Secret key beside withheld values, or one annotation beside the last-applied one. */
  #regionsFor(object, redacted) {
    const protectedPaths = [...MACHINERY, ...redacted];
    const inside = (path) => protectedPaths.some((p) => isPrefix(p, path));
    const holds = (path) => protectedPaths.some((p) => isPrefix(path, p));
    return {
      editable: (path) => !inside(path) && !holds(path) && this.#policy.isEditable(object, path),
      container: (path) => !inside(path) && (this.#policy.containsEditable(object, path) || holds(path) && this.#policy.isEditable(object, path)),
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
function nativeCollectionURL(proxyBase, scope) {
  const q = new URLSearchParams();
  if (scope.labelSelector)
    q.append("labelSelector", scope.labelSelector);
  if (scope.name)
    q.append("fieldSelector", `metadata.name=${scope.name}`);
  const query = q.toString();
  return collectionPath(proxyBase, scope) + (query ? `?${query}` : "");
}
function nativeObjectURL(proxyBase, scope) {
  return `${collectionPath(proxyBase, scope)}/${segment("name", scope.name)}`;
}
function collectionPath(proxyBase, scope) {
  const path = [withoutTrailingSlashes(proxyBase)];
  if (scope.group)
    path.push("apis", segment("group", scope.group));
  else
    path.push("api");
  path.push(segment("version", scope.version));
  if (scope.namespace)
    path.push("namespaces", segment("namespace", scope.namespace));
  path.push(segment("resource", scope.resource));
  return path.join("/");
}
function segment(what, value) {
  if (!value || value === "." || value === ".." || value.includes("/")) {
    throw new Error(`krm-stream: invalid ${what} ${JSON.stringify(value ?? "")}`);
  }
  return encodeURIComponent(value);
}
function withoutTrailingSlashes(base) {
  let end = base.length;
  while (end > 0 && base[end - 1] === "/")
    end--;
  return base.slice(0, end);
}
export {
  DEFAULT_EDITABLE_REGIONS,
  LiveResourceStore,
  PROTOCOL_VERSION,
  SSEDecoder,
  StreamSequence,
  VERSION,
  applyStreamEvent,
  clone,
  connectNativeWatch,
  connectResourceStream,
  deepEqual,
  defaultPolicy,
  get,
  has,
  isPrefix,
  nativeCollectionURL,
  nativeObjectURL,
  parsePointer,
  pathKey,
  readOnlyPolicy,
  regionPolicy,
  resourceStreamURL,
  withOpenAPIKeyedLists
};
