// Observe the same calculated portfolio events that DEGIRO renders and read
// fresh resource snapshots. Only lastData subscriptions and their cleanup are
// sent; broker configuration/secrets are never retained.
// Protocol inspected in trader 5.144.0's event-broker-worker and app bundles.
(() => {
  const state = {
    positions: {}, totals: null, base_currency: "", int_account: "",
    changed_at: 0, totals_at: 0, error: "",
  };
  let generation = 0;
  let readCurrent = null;
  let cancelCurrent = null;
  let sequence = 0;
  // SharedWorker resource replies are broadcast to every port for the client.
  // A page-specific nonce prevents another page's reads matching our attempt.
  const snapshotId = crypto.randomUUID();
  function reset() {
    cancelCurrent?.();
    readCurrent = cancelCurrent = null;
    state.positions = {};
    state.totals = null;
    state.base_currency = "";
    state.int_account = "";
    state.changed_at = 0;
    state.totals_at = 0;
    state.error = "";
  }
  const decode = (data) => {
    if (typeof data === "string") {
      if (data.length > 8 * 1024 * 1024) return null;
      try { return JSON.parse(data); } catch { return null; }
    }
    return data && typeof data === "object" ? data : null;
  };
  const pick = (source, keys) => Object.fromEntries(
    keys.filter((key) => source[key] !== undefined).map((key) => [key, source[key]]),
  );
  function position(row, output = state) {
    if (!row || row.id == null) return;
    if (Object.keys(output.positions).length >= 2000 && !output.positions[row.id]) {
      output.error = "Too many broker positions";
      return;
    }
    const saved = pick(row, ["id", "positionType", "size", "price", "value",
      "valueInProductCurrency", "breakEvenPrice", "lastUpdate",
      "portfolioValueCorrection", "todayPl", "totalPl", "isActive"]);
    if (row.productInfo) saved.productInfo = pick(row.productInfo,
      ["id", "symbol", "name", "isin", "currency", "contractSize",
        "productTypeId", "closePrice", "closePriceDate"]);
    output.positions[String(row.id)] = saved;
  }
  function receive(message, output = state) {
    if (message?.action !== "EMIT_EVENT") return;
    const envelope = message?.data;
    if (!envelope || typeof envelope.event !== "string") return;
    const name = envelope.event, data = envelope.data;
    if (name.startsWith("totalPortfolio:") && data && !Array.isArray(data)) {
      output.totals = pick(data, ["portfolio", "total", "totalCash", "cryptoTotalCash",
        "totalPl", "todayPl", "availableToSpend", "cashFundCompensation",
        "cashFundCompensationPending", "cashFundCompensationCurrency"]);
      output.totals_at = Date.now();
    } else if (name.startsWith("portfolio:") && data) {
      if (name === "portfolio:remove") {
        for (const row of Array.isArray(data) ? data : Object.values(data)) {
          if (row?.id != null) delete output.positions[String(row.id)];
        }
      } else if (data.activePositions) {
        if (envelope.params?.productTypeId == null) output.positions = {};
        Object.values(data.activePositions).forEach((row) => position(row, output));
        Object.values(data.inactivePositions || {}).forEach((row) => position(row, output));
      } else {
        (Array.isArray(data) ? data : Object.values(data)).forEach((row) => position(row, output));
      }
      output.changed_at = Date.now();
    }
  }
  function watch(target, worker) {
    let ownGeneration = 0;
    let clientId = "";
    let pending = null;
    const send = target.postMessage;
    const events = ["portfolio:lastData", "totalPortfolio:lastData"];
    function read() {
      if (pending) return pending.promise;
      const snapshot = {
        positions: {}, totals: null, base_currency: state.base_currency,
        int_account: state.int_account, changed_at: 0, totals_at: 0, error: "",
      };
      if (!clientId || !ownGeneration || ownGeneration !== generation) {
        return Promise.resolve({...snapshot, snapshot_error: "worker_unavailable"});
      }
      const params = {degitideSnapshot: `degitide-${snapshotId}-${ownGeneration}-${++sequence}`};
      let complete;
      const promise = new Promise((resolve) => { complete = resolve; });
      const finish = (reason = "") => {
        if (pending?.promise !== promise) return;
        clearTimeout(pending.timer);
        pending = null;
        if (reason) snapshot.snapshot_error = reason;
        complete(snapshot);
      };
      pending = {promise, params, snapshot, finish, received: new Set(),
        timer: setTimeout(() => finish("timeout"), 1500)};
      const post = (action, event) => Reflect.apply(send, target, [JSON.stringify({
        action, data: {clientId, lastActiveAt: Date.now(), event, params},
      })]);
      try {
        for (const event of events) post("SUBSCRIBE_RESOURCE", event);
      } catch {
        finish("request_failed");
      } finally {
        // The native worker replies synchronously. FIFO on this same port
        // places cleanup after both reads, even when no reply reaches us.
        for (const event of events) {
          try { post("UNSUBSCRIBE_RESOURCE", event); } catch { finish("request_failed"); }
        }
      }
      return promise;
    }
    const invalidate = () => {
      if (!ownGeneration || ownGeneration !== generation) return;
      generation++;
      reset();
      state.error = "Broker portfolio stream ended";
    };
    target.addEventListener("message", (event) => {
      if (!ownGeneration || ownGeneration !== generation) return;
      const message = decode(event.data);
      if (["NOTIFY_ON_EXPIRED_SESSION", "NOTIFY_ON_MAX_FAILED_UPDATE_REQUESTS"].includes(message?.action)) {
        invalidate();
        return;
      }
      const envelope = message?.data;
      if (message?.action === "EMIT_EVENT" && pending &&
          envelope?.params?.degitideSnapshot === pending.params.degitideSnapshot &&
          events.includes(envelope.event) && envelope.params.productTypeId == null) {
        receive(message, pending.snapshot);
        pending.received.add(envelope.event);
        if (pending.received.size === events.length) pending.finish();
      }
      receive(message);
    });
    target.addEventListener("messageerror", invalidate);
    worker.addEventListener("error", invalidate);
    target.postMessage = function (...args) {
      const message = decode(args[0]);
      const config = message?.action === "SUBSCRIBE_MESSAGE_PORT" && message.data?.eventBrokerConfig;
      if (config && !ownGeneration) {
        ownGeneration = ++generation;
        reset();
        state.base_currency = config.currentClient?.accountInfo?.baseCurrency || "";
        state.int_account = String(config.currentClient?.intAccount ?? "");
        clientId = String(message.data.clientId ?? "");
        readCurrent = read;
        cancelCurrent = () => pending?.finish("stream_changed");
      } else if (config || message?.action === "UNSUBSCRIBE_MESSAGE_PORT") {
        // Native trader creates a fresh Worker/port for each start. Reusing one
        // would make old and new event envelopes indistinguishable.
        invalidate();
      }
      return Reflect.apply(send, this, args);
    };
    const closeName = target === worker ? "terminate" : "close";
    const close = target[closeName];
    target[closeName] = function (...args) {
      invalidate();
      return Reflect.apply(close, this, args);
    };
  }
  for (const name of ["Worker", "SharedWorker"]) {
    const Native = window[name];
    if (!Native) continue;
    window[name] = new Proxy(Native, {
      construct(Target, args) {
        const worker = Reflect.construct(Target, args);
        try {
          const url = new URL(String(args[0]), location.href);
          if (url.origin === location.origin &&
              /^\/trader\/scripts\/event-broker-worker\.[a-f0-9]+\.js$/.test(url.pathname)) {
            watch(name === "SharedWorker" ? worker.port : worker, worker);
          }
        } catch { /* An unrelated worker does not affect portfolio capture. */ }
        return worker;
      },
    });
  }
  Object.defineProperty(window, "__degitidePortfolio", { value: state });
  Object.defineProperty(window, "__degitideReadPortfolio", { value: () => readCurrent
    ? readCurrent() : Promise.resolve({...state, positions: {}, totals: null, snapshot_error: "worker_unavailable"}),
  });
})();
