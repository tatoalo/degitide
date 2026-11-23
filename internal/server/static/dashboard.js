"use strict";
const $ = (id) => document.getElementById(id);
const state = {
  account: "default",
  view: "overview",
  range: "ALL",
  accounts: [],
  transactions: [],
  history: [],
  valuation: null,
  previousSessionDayPL: null,
  status: null,
  generation: 0,
  polling: false,
  wasRunning: false,
  sync: null,
  syncRevision: 0,
  syncPending: false,
  syncStreamConnected: false,
};
let currentHoldings = [],
  selectedCurrency = "";
let connectAccount = "default",
  pollTimer,
  progressDismissTimer;
const completedBannerDuration = 5000;
const escapeHTML = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const formatNumber = (value, digits = 2) =>
  Number.isFinite(Number(value))
    ? new Intl.NumberFormat(undefined, {
        maximumFractionDigits: digits,
      }).format(Number(value))
    : "—";
function formatMoney(value, currency) {
  value = roundedMoney(value);
  if (value == null) return "—";
  try {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency,
      maximumFractionDigits: 2,
    }).format(Number(value));
  } catch {
    return formatNumber(value, 2) + " " + (currency || "(currency unknown)");
  }
}
function formatProfit(value, currency) {
  return (moneyDirection(value) === "gain" ? "+" : "") + formatMoney(value, currency);
}
function formatDate(value, withTime = false) {
  if (!value) return "Never";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "Unknown";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    ...(withTime ? { timeStyle: "short" } : {}),
  }).format(date);
}
function endpoint(path, account = state.account) {
  return path + "?account=" + encodeURIComponent(account);
}
async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status === 401) location.assign("/login");
    if (response.status === 503 && body.error === "dashboard setup required")
      location.assign("/setup");
    const error = Error(body.error || "Request failed. Try again.");
    error.status = response.status;
    error.code = body.code;
    throw error;
  }
  return body;
}
function showNotice(message, error = false) {
  $("notice").textContent = message;
  $("notice").hidden = !message;
  $("notice").className = "notice" + (error ? " error" : "");
}
async function refreshVersions() {
  const root = $("versions");
  try {
    const versions = await api("/api/versions");
    $("changelogVersion").textContent = "Installed · " + (versions.app || "Unknown version");
    root.replaceChildren();
    for (const [label, value] of [
      ["App", versions.app || "unknown"],
      ["Worker", versions.worker?.version || "unavailable"],
      ["Patchright", versions.worker?.patchright],
      ["Python", versions.worker?.python],
    ]) {
      if (!value) continue;
      const item = document.createElement("span"),
        number = document.createElement("b");
      number.textContent =
        value === "dev" || /^[a-f0-9]{40}$/i.test(value)
          ? "Development"
          : value;
      number.title = value;
      item.append(label + " ", number);
      root.appendChild(item);
    }
  } catch {
    root.textContent = "Versions unavailable";
  }
}
function cell(row, text, className = "") {
  const td = document.createElement("td");
  td.textContent = text;
  td.className = className;
  row.appendChild(td);
  return td;
}
function showView(view) {
  state.view = view;
  document.body.dataset.currentView = view;
  document
    .querySelectorAll("[data-pages]")
    .forEach((e) => (e.hidden = !e.dataset.pages.split(" ").includes(view)));
  document.querySelectorAll("#appNav [data-view]").forEach((b) => {
    if (b.dataset.view === view) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
  });
  const copy = {
    overview: ["Portfolio overview", "What you own, in one place."],
    holdings: [
      "Your holdings",
      "Every position in this account, grouped by currency.",
    ],
    transactions: ["Transactions", "Your latest 100 saved transactions."],
    simulator: [
      "Explore a scenario",
      "Keep assumptions separate from your recorded portfolio.",
    ],
    accounts: [
      "Your accounts",
      "Connect once. Let your server keep them up to date.",
    ],
  };
  $("pageTitle").textContent = copy[view][0];
  $("pageDescription").textContent = copy[view][1];
  renderTransactions();
  renderHistory(false);
  renderSchedule(state.status || {});
  renderSimulation();
  if (view === "accounts") loadSureConnections();
}
function renderAccounts() {
  const select = $("accountSelect");
  select.replaceChildren();
  for (const account of state.accounts) {
    const option = document.createElement("option");
    option.value = account.id;
    option.textContent = account.name;
    select.appendChild(option);
  }
  select.value = state.account;
  const selected = state.accounts.find((a) => a.id === state.account);
  $("accountName").textContent = selected?.name || "Your portfolio";
  $("accountList").innerHTML = state.accounts
    .map(
      (a) =>
        `<article class="account-row"><div><h3>${escapeHTML(a.name)}</h3><p>${escapeHTML(a.id)} · Last sync: ${escapeHTML(formatDate(a.last_pull, true))}</p>${a.last_error ? `<p>${escapeHTML(a.last_error)}</p>` : ""}</div><div class="account-actions"><span class="pill ${a.last_error ? "bad" : a.connected ? "" : "neutral"}">${a.last_error ? "Needs attention" : a.connected ? "Connected" : "Not connected"}</span><button class="button" data-connect="${escapeHTML(a.id)}">${a.connected ? "Reconnect" : "Connect DEGIRO"}</button><button class="text-button" data-select-account="${escapeHTML(a.id)}">View →</button></div></article>`,
    )
    .join("");
}
// Issuer logo (or a text badge) for a holding or transaction row.
function issuerMark(item) {
  const issuer = fundIssuer(item);
  if (!issuer) return null;
  if (issuer.logo) {
    const img = document.createElement("img");
    img.src = "/logos/" + issuer.id + ".svg";
    img.alt = issuer.label;
    img.title = issuer.label;
    img.className = "issuer-logo";
    img.decoding = "async";
    return img;
  }
  const badge = document.createElement("span");
  badge.className = "issuer-badge";
  badge.textContent = issuer.label;
  return badge;
}
const holdingCurrency = (h) => h.value_currency || h.currency;
function renderHoldings() {
  const body = $("holdingsBody");
  body.replaceChildren();
  for (const h of currentHoldings) {
    const row = document.createElement("tr");
    const symbol = cell(row, h.symbol || h.product_id || "Unknown", "symbol");
    const mark = issuerMark(h);
    if (mark) symbol.prepend(mark);
    const currency = document.createElement("small");
    currency.textContent = [h.currency || "Currency unknown", h.exchange_mic].filter(Boolean).join(" · ");
    currency.title = h.exchange_name || "Trading venue not yet identified";
    symbol.appendChild(currency);
    cell(row, formatNumber(h.quantity, 6), "number");
    cell(row, formatMoney(h.avg_price, h.currency), "number");
    const market = cell(row, formatMoney(h.market_value, holdingCurrency(h)), "number");
    const asOf = document.createElement("small");
    asOf.textContent = h.price_as_of
      ? `${h.valuation_source === "broker_quote" ? "Quote" : "Close"} · ${h.price_as_of.includes("T") ? new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "short"}).format(new Date(h.price_as_of)) : formatDate(h.price_as_of)}`
      : "Price time unavailable";
    asOf.title = h.price_as_of || "The sync time is not the price time.";
    market.appendChild(asOf);
    body.appendChild(row);
  }
  $("holdingSummary").textContent = currentHoldings.length + " positions";
  $("holdingsEmpty").hidden = currentHoldings.length !== 0;
}
function renderTransactions() {
  const overview = state.view === "overview";
  const items = overview ? state.transactions.slice(0, 3) : state.transactions;
  const body = $("transactionsBody");
  body.replaceChildren();
  const names = new Map(currentHoldings.map((h) => [h.product_id, h.name]));
  for (const item of items) {
    const row = document.createElement("tr");
    cell(row, formatDate(item.date));
    const symbol = cell(
      row,
      item.symbol || item.product_id || "Unknown",
      "symbol",
    );
    const mark = issuerMark({ ...item, name: names.get(item.product_id) });
    if (mark) symbol.prepend(mark);
    const type = cell(row, "");
    const badge = document.createElement("span");
    badge.className = "pill neutral";
    badge.textContent = item.type || "—";
    type.appendChild(badge);
    cell(row, formatNumber(item.quantity, 6), "number");
    cell(row, formatMoney(item.price, item.currency), "number");
    body.appendChild(row);
  }
  $("transactionsHeading").textContent = overview
    ? "Recent transactions"
    : "Transactions";
  $("allTransactionsButton").hidden = !overview;
  $("transactionSummary").hidden = overview;
  $("transactionSummary").textContent = items.length + " shown";
  $("transactionsEmpty").hidden = items.length !== 0;
  $("transactionExports").hidden = overview;
  document.querySelectorAll("[data-export]").forEach((link) => {
    link.href =
      endpoint("/api/transactions/export") + "&format=" + link.dataset.export;
  });
}
function renderCurrencies() {
  const select = $("chartCurrency");
  const currencies = [
    ...new Set([
      ...currentHoldings.map(holdingCurrency),
      ...(state.valuation?.base_currency ? [state.valuation.base_currency] : []),
      ...state.history.map((p) => p.currency),
    ]),
  ].sort();
  select.replaceChildren();
  for (const currency of currencies) {
    const option = document.createElement("option");
    option.value = currency;
    option.textContent = currency || "Unknown";
    select.appendChild(option);
  }
  if (currencies.includes(selectedCurrency)) select.value = selectedCurrency;
  selectedCurrency = select.value;
  select.disabled = !currencies.length;
}
function renderAllocation() {
  const matching = currentHoldings.filter(
    (h) => holdingCurrency(h) === selectedCurrency && Number(h.market_value) > 0,
  );
  const total = matching.reduce((sum, h) => sum + Number(h.market_value), 0);
  $("allocationCurrency").textContent = selectedCurrency
    ? selectedCurrency + " positions"
    : "";
  $("allocationBar").innerHTML = matching
    .map(
      (h, i) =>
        `<i style="width:${(h.market_value / total) * 100}%;opacity:${Math.max(0.3, 1 - i * 0.14)}"></i>`,
    )
    .join("");
  $("allocationList").innerHTML = matching
    .map(
      (h, i) =>
        `<div class="allocation-item"><span><i class="allocation-dot" style="opacity:${Math.max(0.3, 1 - i * 0.14)}"></i>${escapeHTML(h.symbol || h.product_id)}</span><span class="number">${((h.market_value / total) * 100).toFixed(1)}%</span></div>`,
    )
    .join("");
  const other = currentHoldings.filter(
    (h) => holdingCurrency(h) !== selectedCurrency,
  ).length;
  $("allocationNote").textContent = !matching.length
    ? "No positive positions in this currency yet."
    : `Share of positive holdings in ${selectedCurrency || "the selected currency"}.` +
      (other
        ? ` ${other} positions in other currencies are shown separately.`
        : "");
}
function drawChart(target, points, baseline, xLabel) {
  const root = $(target);
  root.replaceChildren();
  if (!points.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "History starts with your first saved snapshot.";
    root.appendChild(empty);
    return;
  }
  const history = target === "historyChart";
  const width = Math.max(260, root.clientWidth || 520),
    height = 250,
    left = 58,
    right = 12,
    top = 18,
    bottom = 37;
  const values = points
    .map((p) => p.value)
    .concat((baseline || []).filter(Number.isFinite));
  const min = Math.min(...values),
    max = Math.max(...values),
    padding = Math.max(1, (max - min) * 0.12);
  const low = min >= 0 ? Math.max(0, min - padding) : min - padding,
    high = Math.max(low + 1, max + padding);
  const first = points[0].x,
    last = points.at(-1).x;
  const x = (value) =>
    left +
    (last === first ? 0.5 : (value - first) / (last - first)) *
      (width - left - right);
  const y = (value) =>
    height - bottom - ((value - low) / (high - low)) * (height - top - bottom);
  const ns = "http://www.w3.org/2000/svg",
    svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("role", "group");
  svg.setAttribute("aria-roledescription", "interactive line chart");
  svg.setAttribute("tabindex", "0");
  svg.setAttribute(
    "aria-label",
    (history
      ? "Portfolio value and net invested in "
      : "Hypothetical portfolio value in ") + selectedCurrency,
  );
  svg.setAttribute("aria-describedby", target + "Help " + target + "Readout");
  function el(name, attrs, text) {
    const n = document.createElementNS(ns, name);
    for (const [key, value] of Object.entries(attrs))
      n.setAttribute(key, value);
    if (text !== undefined) n.textContent = text;
    svg.appendChild(n);
    return n;
  }
  for (let i = 0; i < 3; i++) {
    const value = low + ((high - low) * i) / 2;
    el("line", {
      x1: left,
      x2: width - right,
      y1: y(value),
      y2: y(value),
      stroke: "var(--line)",
      "stroke-width": 0.8,
    });
    el(
      "text",
      {
        x: left - 10,
        y: y(value) + 4,
        "text-anchor": "end",
        fill: "var(--muted)",
        "font-size": 11,
      },
      new Intl.NumberFormat(undefined, {
        notation: "compact",
        maximumFractionDigits: 1,
      }).format(value),
    );
  }
  function line(vals, color, dashed = false) {
    let connected = false;
    const path = points
      .map((p, i) => {
        if (!Number.isFinite(vals[i])) {
          connected = false;
          return "";
        }
        const segment = `${connected ? "L" : "M"}${x(p.x)},${y(vals[i])}`;
        connected = true;
        return segment;
      })
      .join(" ");
    el("path", {
      d: path,
      fill: "none",
      stroke: color,
      "stroke-width": dashed ? 1.8 : 2.5,
      "stroke-dasharray": dashed ? "5 5" : "none",
      "stroke-linejoin": "round",
      "stroke-linecap": "round",
    });
  }
  if (baseline) line(baseline, "#80958d", true);
  line(
    points.map((p) => p.value),
    "var(--accent)",
  );
  if (points.length <= 12) {
    for (const p of points)
      el("circle", { cx: x(p.x), cy: y(p.value), r: 3, fill: "var(--accent)" });
  }
  el(
    "text",
    { x: left, y: height - 8, fill: "var(--muted)", "font-size": 11 },
    xLabel(first),
  );
  if (last !== first)
    el(
      "text",
      {
        x: width - right,
        y: height - 8,
        "text-anchor": "end",
        fill: "var(--muted)",
        "font-size": 11,
      },
      xLabel(last),
    );
  const guide = el("line", {
    y1: top,
    y2: height - bottom,
    stroke: "var(--accent)",
    "stroke-opacity": 0.4,
    "stroke-dasharray": "3 4",
    visibility: "hidden",
  });
  const marker = el("circle", {
    r: 5,
    fill: "var(--accent)",
    stroke: "var(--surface)",
    "stroke-width": 2,
    visibility: "hidden",
  });
  const baseMarker = el("circle", {
    r: 4,
    fill: "#80958d",
    stroke: "var(--surface)",
    "stroke-width": 2,
    visibility: "hidden",
  });
  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.hidden = true;
  tooltip.setAttribute("aria-hidden", "true");
  const date = document.createElement("p");
  date.className = "chart-tooltip-date";
  tooltip.appendChild(date);
  const rows = [
    "Portfolio value",
    history ? "Net invested" : "Contributions",
    history ? "Trading P&L" : "Simulated growth",
  ].map((label) => {
    const row = document.createElement("div"),
      name = document.createElement("span"),
      value = document.createElement("strong");
    row.className = "chart-tooltip-row";
    name.textContent = label;
    row.append(name, value);
    tooltip.appendChild(row);
    return { row, value, label };
  });
  const help = document.createElement("p");
  help.id = target + "Help";
  help.className = "sr-only";
  help.textContent =
    "Hover or touch to inspect values. Use Left and Right arrows to move between points, Home and End for the first and last point, and Escape to dismiss.";
  const readout = document.createElement("p");
  readout.id = target + "Readout";
  readout.className = "sr-only";
  readout.setAttribute("aria-live", "polite");
  readout.setAttribute("aria-atomic", "true");
  root.append(svg, tooltip, help, readout);
  let active = points.length - 1;
  function show(index, announce = false) {
    active = Math.max(0, Math.min(points.length - 1, index));
    const p = points[active],
      bx = x(p.x),
      invested = baseline?.[active];
    date.textContent = history
      ? new Intl.DateTimeFormat(undefined, {
          dateStyle: "medium",
          timeZone: "UTC",
        }).format(new Date(p.x))
      : xLabel(p.x);
    rows[0].value.textContent = formatMoney(p.value, selectedCurrency);
    const hasBaseline = Number.isFinite(invested);
    rows[1].row.hidden = rows[2].row.hidden = !hasBaseline;
    if (hasBaseline) {
      const profit = p.value - invested;
      rows[1].value.textContent = formatMoney(invested, selectedCurrency);
      rows[2].value.textContent = formatProfit(profit, selectedCurrency);
      rows[2].value.className = moneyDirection(profit);
      baseMarker.setAttribute("cx", bx);
      baseMarker.setAttribute("cy", y(invested));
    }
    guide.setAttribute("x1", bx);
    guide.setAttribute("x2", bx);
    marker.setAttribute("cx", bx);
    marker.setAttribute("cy", y(p.value));
    guide.setAttribute("visibility", "visible");
    marker.setAttribute("visibility", "visible");
    baseMarker.setAttribute("visibility", hasBaseline ? "visible" : "hidden");
    tooltip.hidden = false;
    const scale = svg.getBoundingClientRect().width / width;
    const anchor = bx * scale,
      tooltipWidth = tooltip.offsetWidth;
    const preferred =
      anchor + 16 + tooltipWidth <= root.clientWidth
        ? anchor + 16
        : anchor - tooltipWidth - 16;
    tooltip.style.left =
      Math.max(8, Math.min(root.clientWidth - tooltipWidth - 8, preferred)) +
      "px";
    tooltip.style.top = "8px";
    if (announce)
      readout.textContent =
        date.textContent +
        ". " +
        rows
          .filter((r) => !r.row.hidden)
          .map((r) => r.label + ": " + r.value.textContent)
          .join(". ");
  }
  function hide() {
    tooltip.hidden = true;
    for (const node of [guide, marker, baseMarker])
      node.setAttribute("visibility", "hidden");
  }
  function inspect(event) {
    const bounds = svg.getBoundingClientRect();
    const localX = ((event.clientX - bounds.left) * width) / bounds.width;
    const localY = ((event.clientY - bounds.top) * height) / bounds.height;
    if (
      localX < left ||
      localX > width - right ||
      localY < top ||
      localY > height - bottom
    ) {
      hide();
      return;
    }
    let nearest = 0;
    for (let i = 1; i < points.length; i++) {
      if (
        Math.abs(x(points[i].x) - localX) <
        Math.abs(x(points[nearest].x) - localX)
      )
        nearest = i;
    }
    show(nearest);
  }
  svg.addEventListener("pointermove", inspect);
  svg.addEventListener("pointerdown", inspect);
  svg.addEventListener("pointerleave", hide);
  svg.addEventListener("pointercancel", hide);
  svg.addEventListener("focus", () => show(active, true));
  svg.addEventListener("blur", hide);
  svg.addEventListener("keydown", (event) => {
    const index = {
      ArrowLeft: active - 1,
      ArrowRight: active + 1,
      Home: 0,
      End: points.length - 1,
    }[event.key];
    if (index !== undefined) {
      event.preventDefault();
      show(index, true);
    } else if (event.key === "Escape") {
      event.preventDefault();
      hide();
    }
  });
}
function renderBrokerValuation() {
  const valuation = state.valuation;
  const visible = valuation && valuation.base_currency === selectedCurrency;
  $("brokerSummary").hidden = !visible;
  $("brokerReconciliation").hidden = true;
  $("valuationNote").textContent = visible
    ? (valuation.warning || (valuation.price_status === "quoted"
      ? "DEGIRO quote valuation. Individual prices may be delayed; see holding timestamps."
      : "DEGIRO closing valuation. See holding timestamps."))
    : "Recorded holdings. Price times and broker account totals need a new sync.";
  $("valuationSummary").textContent = visible
    ? (valuation.price_status === "quoted" ? "Broker quotes · may be delayed"
      : valuation.price_status === "closing" ? "Includes closing prices" : "Quote valuation unavailable")
    : "Recorded prices · freshness unavailable";
  if (!visible) return;
  const previous = state.previousSessionDayPL?.currency === valuation.base_currency
    ? state.previousSessionDayPL : null;
  $("brokerDayLabel").textContent = previous ? "Previous session P&L" : "Day P&L";
  for (const [id, key] of [["brokerPortfolio", "portfolio_value"], ["brokerCash", "cash_balance"],
    ["brokerBalance", "account_value"], ["brokerPL", "total_pl"], ["brokerDayPL", "day_pl"]]) {
    const value = key === "day_pl" && previous ? previous.value : valuation[key];
    const element = $(id);
    element.textContent = formatMoney(value, valuation.base_currency);
    if (key === "total_pl" || key === "day_pl") {
      const direction = moneyDirection(value);
      element.textContent = formatProfit(value, valuation.base_currency);
      const metric = element.closest("[data-metric]");
      metric.dataset.direction = direction;
      metric.querySelector(".broker-direction").textContent = {
        gain: "↗", loss: "↘", neutral: "→", unavailable: "",
      }[direction];
    }
  }
  $("brokerDayContext").textContent = previous
    ? previous.recorded_at ? "Recorded · " + formatDate(previous.recorded_at, true)
      : calendarDate(previous.date) + " · no saved result"
    : valuation.fetched_at ? "At sync · " + formatDate(valuation.fetched_at, true) : "At the saved sync";
  $("brokerReconciliation").hidden = valuation.reconciliation_difference == null ||
    Math.abs(valuation.reconciliation_difference) < 0.02;
  $("brokerReconciliation").textContent = "Difference from listed securities: " +
    formatMoney(valuation.reconciliation_difference, valuation.base_currency);
}

function renderHistory(resetSimulation = true) {
  selectedCurrency = $("chartCurrency").value;
  let points = state.history
    .filter((p) => p.currency === selectedCurrency)
    .map((p) => ({
      x: Date.parse(p.date.slice(0, 10)),
      value: Number(p.value),
      invested: Number.isFinite(p.net_invested) ? p.net_invested : null,
      profit: Number.isFinite(p.profit_loss) ? p.profit_loss : null,
    }))
    .filter((p) => Number.isFinite(p.x) && Number.isFinite(p.value));
  const all = points;
  const newest = points.at(-1)?.x;
  if (newest && state.range !== "ALL") {
    const months = { "1M": 1, "3M": 3, "1Y": 12 }[state.range] || 3;
    const start = new Date(newest);
    start.setUTCMonth(start.getUTCMonth() - months);
    points = points.filter((p) => p.x >= start.getTime());
  }
  // Reconstructed history spans years, so show the year once it matters.
  const longRange =
    points.length > 1 && points.at(-1).x - points[0].x > 300 * 86400000;
  const baseline = points.map((p) => p.invested);
  const hasPerformance = baseline.some(Number.isFinite);
  $("investedLegend").hidden = !hasPerformance;
  drawChart("historyChart", points, hasPerformance ? baseline : null, (x) =>
    new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "short",
      ...(longRange ? { year: "numeric" } : {}),
      timeZone: "UTC",
    }).format(new Date(x)),
  );
  const matching = currentHoldings.filter(
    (h) => holdingCurrency(h) === selectedCurrency,
  );
  const value = matching.reduce((sum, h) => sum + Number(h.market_value), 0);
  $("portfolioValue").textContent = currentHoldings.length || state.valuation || state.accounts.some((a) => a.id === state.account && a.last_pull)
    ? formatMoney(value, selectedCurrency)
    : "—";
  $("portfolioContext").textContent =
    matching.length +
    " positions" +
    (selectedCurrency ? " · " + selectedCurrency : "");
  renderBrokerValuation();
  const latest = all.at(-1);
  const hasLatestPerformance =
    Number.isFinite(latest?.invested) && Number.isFinite(latest?.profit);
  $("performanceSummary").hidden = !hasLatestPerformance;
  $("performanceAsOf").hidden = !all.length;
  if (hasLatestPerformance) {
    $("netInvested").textContent = formatMoney(
      latest.invested,
      selectedCurrency,
    );
    $("profitLoss").textContent = formatProfit(latest.profit, selectedCurrency);
    $("profitLoss").className = moneyDirection(latest.profit);
    $("performanceAsOf").textContent =
      "Snapshot · " +
      new Intl.DateTimeFormat(undefined, {
        dateStyle: "medium",
        timeZone: "UTC",
      }).format(new Date(latest.x));
  } else {
    $("performanceAsOf").textContent =
      "Net invested and trading P&L need complete trade history and matching historical currencies.";
  }
  const reported = state.history.filter(
    (p) => p.currency === selectedCurrency && p.source === "report",
  ).length;
  $("historyNote").textContent =
    all.length === 1
      ? "One snapshot saved. Future syncs will build your history."
      : reported
        ? reported +
          " weekly values from DEGIRO position reports (that day's close), " +
          (all.length - reported) +
          " synced snapshots."
        : all.length + " saved snapshots.";
  if (resetSimulation) {
    $("simInitial").value = value.toFixed(2);
    prefillMonthlyContribution();
    renderSimulation();
  }
  renderAllocation();
  renderReturnHelp();
}
// Default the monthly contribution to the selected currency's recent net buys.
function prefillMonthlyContribution() {
  const average = averageMonthlyContribution(
    state.transactions,
    selectedCurrency,
  );
  const note = $("simMonthlyNote");
  if (!average) {
    note.textContent = "";
    return;
  }
  $("simMonthly").value = average.monthly.toFixed(2);
  note.textContent =
    "Prefilled with your average net buys per month over the last " +
    (average.months === 1 ? "month" : average.months + " months") +
    " (" +
    average.trades +
    (average.trades === 1 ? " trade" : " trades") +
    "). Edit freely.";
}
function renderSimulation() {
  try {
    const ids = ["simInitial", "simMonthly", "simRate", "simYears"];
    if (ids.some((id) => !$(id).checkValidity() || $(id).value === ""))
      throw Error("Check the simulation inputs.");
    const [initial, monthly, rate, years] = ids.map((id) =>
      Number($(id).value),
    );
    const points = projectPortfolio(initial, monthly, rate, years),
      last = points.at(-1);
    $("simResult").textContent =
      formatMoney(last.value, selectedCurrency) + " after " + years + " years";
    $("simBreakdown").textContent =
      "Starting value + contributions: " +
      formatMoney(last.contributed, selectedCurrency) +
      " · Simulated growth: " +
      formatMoney(last.value - last.contributed, selectedCurrency);
    drawChart(
      "simulationChart",
      points,
      points.map((p) => p.contributed),
      (x) => "Year " + x,
    );
  } catch (error) {
    $("simResult").textContent = error.message;
    $("simulationChart").replaceChildren();
    $("simBreakdown").textContent = "";
  }
}
function renderLastSync(account) {
  const timestamp = account?.last_pull;
  const date = timestamp ? new Date(timestamp) : null;
  const known = date && Number.isFinite(date.getTime()) && date.getUTCFullYear() > 1;
  const formatted = known
    ? new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "long"}).format(date)
    : "";
  $("lastSync").textContent = known ? "Last sync · " + formatted : "No saved snapshot yet";
  $("summaryLastSync").hidden = !known;
  const time = $("summarySyncTime");
  time.textContent = formatted;
  time.title = known ? timestamp : "";
  if (known) time.dateTime = timestamp;
  else time.removeAttribute("datetime");
}
function renderSyncControls() {
  const running = !!state.sync?.pull_running;
  $("pullButton").disabled = $("syncAllButton").disabled = running || state.syncPending;
  $("pullButton").textContent = running ? "↻ Syncing…" : state.syncPending ? "↻ Starting sync…" : "↻ Sync account";
}
function renderStatus(status, syncRevision = state.syncRevision) {
  // A live event received during an HTTP request is newer than its response.
  if (syncRevision === state.syncRevision) {
    state.sync = { pull_running: !!status.pull_running, progress: status.progress };
    state.syncRevision++;
  }
  status = { ...status, ...state.sync };
  if (status.pull_running && !state.wasRunning) showNotice("");
  state.status = status;
  const account = status.account || {};
  $("connectionStatus").className =
    "pill " + (account.last_error ? "bad" : account.connected ? "" : "neutral");
  $("connectionStatus").textContent = account.last_error
    ? "Needs attention"
    : account.connected
      ? "Connected"
      : "Not connected";
  renderLastSync(account);
  renderSchedule(status);
  renderSyncControls();
  const progress = status.progress;
  const hasRun =
    progress?.started_at && !progress.started_at.startsWith("0001");
  clearTimeout(progressDismissTimer);
  const failed = (progress?.accounts || []).some((a) => a.state === "failed");
  const completed =
    !status.pull_running && progress?.percent === 100 && !failed;
  const finishedAt = Date.parse(progress?.finished_at);
  const remaining = completedBannerDuration - (Date.now() - finishedAt);
  $("syncProgress").hidden =
    !hasRun || (completed && (!Number.isFinite(remaining) || remaining <= 0));
  if (completed && remaining > 0) {
    const run = progress.started_at;
    progressDismissTimer = setTimeout(() => {
      if (
        !state.status?.pull_running &&
        state.status?.progress?.started_at === run
      )
        $("syncProgress").hidden = true;
    }, remaining);
  }
  if (hasRun) {
    $("syncProgress").className = "sync-progress" + (failed ? " failed" : "");
    $("progressLabel").textContent = progress.stage;
    $("progressValue").textContent = progress.percent + "%";
    $("progressBar").value = progress.percent;
    $("progressAccounts").innerHTML = (progress.accounts || [])
      .map(
        (a) =>
          `<span class="progress-account ${a.state === "failed" ? "failed" : ""}">${a.state === "complete" ? "✓" : a.state === "failed" ? "!" : a.state === "running" ? "↻" : "·"} ${escapeHTML(a.name)} · ${escapeHTML(a.stage && a.state === "running" ? a.stage : a.state)}</span>`,
      )
      .join("");
  }
  const idx = state.accounts.findIndex((a) => a.id === account.id);
  if (idx >= 0) state.accounts[idx] = account;
  if (account.last_error && !status.pull_running)
    showNotice(account.last_error, true);
  state.wasRunning = !!status.pull_running;
}
function calendarDate(value) {
  return new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeZone: "UTC"})
    .format(new Date(value + "T00:00:00Z"));
}
// Browser time with its zone, plus the server's clock when that reads differently.
function browserAndServerTime(value, serverZone) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "Unknown";
  const day = { day: "numeric", month: "short" }, time = { hour: "2-digit", minute: "2-digit" };
  const format = (options) => new Intl.DateTimeFormat(undefined, options).format(date);
  const local = format({ ...day, year: "numeric", ...time, timeZoneName: "short" });
  if (!serverZone) return local;
  try {
    const sameDay = format({ ...day, timeZone: serverZone }) === format(day);
    if (sameDay && format({ ...time, timeZone: serverZone }) === format(time)) return local;
    return local + " · server " + format({ ...(sameDay ? {} : day), ...time, timeZone: serverZone, timeZoneName: "short" });
  } catch {
    return local; // An unknown server zone must not hide the browser time.
  }
}
function scheduleRunTime(at, timezone) {
  const offset = timezone === "Server time" ? " UTC" + (at.endsWith("Z") ? "+00:00" : at.slice(-6)) : "";
  return calendarDate(at.slice(0, 10)) + " · " + at.slice(11, 16) + offset;
}
function describeCustomSchedule(schedule) {
  const spec = (schedule.cron || "").replace(/^(?:CRON_TZ|TZ)=\S+\s+/, "");
  const [minute, hour, day, month, weekday] = spec.split(/\s+/);
  let description = {"@hourly": "Every hour", "@daily": "Every day at 00:00", "@midnight": "Every day at 00:00",
    "@weekly": "Sundays at 00:00", "@monthly": "First day of each month at 00:00", "@yearly": "January 1 at 00:00", "@annually": "January 1 at 00:00"}[spec] || "Custom cron";
  const days = {"*": "Every day", "1-5": "Weekdays", "MON-FRI": "Weekdays", "0,6": "Weekends", "6,0": "Weekends"}[weekday?.toUpperCase()];
  if (day === "*" && month === "*" && days) {
    const clock = (h, m) => String(h).padStart(2, "0") + ":" + String(m).padStart(2, "0");
    const step = minute === "*" ? 1 : Number(minute?.match(/^\*\/(\d+)$/)?.[1]);
    const cadence = /^\d+$/.test(minute) ? (Number(minute) === 0 ? "hourly" : "hourly at :" + minute.padStart(2, "0"))
      : step > 0 && 60 % step === 0 ? step === 1 ? "every minute" : `every ${step} min` : "";
    if (/^\d+$/.test(minute) && /^\d+$/.test(hour)) description = days + " at " + clock(hour, minute);
    else if (cadence && hour === "*") description = days + " · " + cadence;
    else if (cadence && /^\d+-\d+$/.test(hour)) {
      const [first, last] = hour.split("-");
      const firstMinute = step ? 0 : Number(minute), lastMinute = step ? 60 - step : Number(minute);
      description = days + " · " + cadence + ", " + clock(first, firstMinute) + "–" + clock(last, lastMinute);
    }
  }
  return description + (schedule.timezone ? " · " + schedule.timezone : "");
}
function renderSchedule(status) {
  const schedule = status.schedule;
  $("marketSchedule").hidden = !schedule || state.view !== "overview";
  if (!schedule) return;
  const markets = schedule.markets || [];
  const open = markets.some(m => m.open);
  const known = schedule.calendar_available && markets.length > 0;
  const label = $("marketState");
  label.textContent = schedule.mode === "manual" ? "Automatic sync paused" : schedule.mode === "cron" ? "Custom schedule"
    : known ? (open ? "Markets open" : schedule.unknown_holdings ? "Known markets closed" : "Markets closed")
    : schedule.reason === "Identify held markets" ? "Learning markets" : "Daily fallback";
  label.dataset.open = String(open);
  $("marketVenues").textContent = schedule.mode !== "market" ? "" : markets.map(m => m.name + " (" + m.mic + ")").join(" · ");
  $("marketCadence").textContent = schedule.mode === "cron" ? describeCustomSchedule(schedule) : schedule.mode !== "market" ? ""
    : known ? `${schedule.interval_minutes === 60 ? "Hourly" : `Every ${schedule.interval_minutes} min`} during regular sessions · final sync ${schedule.close_delay_minutes ?? 20} min after close`
    : "Trading calendar unavailable. A daily safety refresh keeps the saved snapshot up to date.";
  $("marketFallback").textContent = schedule.unknown_holdings
    ? `${schedule.unknown_holdings} holding${schedule.unknown_holdings === 1 ? " uses" : "s use"} the daily fallback; their trading hours are unknown.` : "";
  $("nextSync").textContent = status.pull_running ? "Sync in progress"
    : schedule.next_sync ? "Next sync · " + (schedule.mode === "cron"
      ? scheduleRunTime(schedule.next_sync, schedule.timezone) : browserAndServerTime(schedule.next_sync, schedule.server_timezone)) : schedule.reason;
  $("nextSyncReason").textContent = schedule.next_sync && !status.pull_running
    ? schedule.mode === "cron" ? schedule.timezone || "" : schedule.reason : "";
  $("nextSync").title = schedule.next_sync || "";
  $("scheduleWindow").hidden = schedule.mode !== "cron";
  $("scheduleExpression").textContent = schedule.cron || "";
  $("scheduleRuns").replaceChildren(...(schedule.upcoming_runs || []).map(at => {
    const item = document.createElement("li"), time = document.createElement("time");
    time.dateTime = at;
    time.title = at;
    // These timestamps carry the scheduler's timezone offset. Display its wall
    // clock consistently with the cron expression, not the browser's timezone.
    time.textContent = scheduleRunTime(at, schedule.timezone);
    item.append(time);
    return item;
  }));
}
async function refreshAll() {
  refreshVersions();
  const generation = ++state.generation,
    account = state.account,
    syncRevision = state.syncRevision;
  $("refreshButton").disabled = true;
  try {
    const [accounts, status, snapshot, returns] =
      await Promise.all([
        api("/api/accounts"),
        api(endpoint("/api/status", account)),
        api(endpoint("/api/portfolio/snapshot", account)),
        api(endpoint("/api/portfolio/returns", account)),
      ]);
    if (generation !== state.generation) return;
    state.accounts = accounts;
    currentHoldings = snapshot.holdings || [];
    state.transactions = snapshot.transactions || [];
    state.history = snapshot.history || [];
    state.valuation = snapshot.valuation || null;
    state.previousSessionDayPL = snapshot.previous_session_day_pl || null;
    state.returns = returns || [];
    showNotice("");
    renderAccounts();
    renderHoldings();
    renderTransactions();
    renderCurrencies();
    renderHistory();
    renderStatus(status, syncRevision);
  } catch (error) {
    if (generation === state.generation) showNotice(error.message, true);
  } finally {
    if (generation === state.generation) {
      $("refreshButton").disabled = false;
      schedulePoll();
    }
  }
}
function schedulePoll() {
  clearTimeout(pollTimer);
  // Keep account data/schedules fresh; use fast polling if streaming is down.
  pollTimer = setTimeout(pollStatus, state.syncStreamConnected ? 30000 : 1300);
}
async function pollStatus() {
  if (state.polling) {
    schedulePoll();
    return;
  }
  state.polling = true;
  const account = state.account,
    generation = state.generation,
    syncRevision = state.syncRevision;
  try {
    const status = await api(endpoint("/api/status", account));
    if (generation !== state.generation) return;
    const finished = state.wasRunning && !status.pull_running;
    const updated = status.account?.last_pull !== state.status?.account?.last_pull;
    renderStatus(status, syncRevision);
    if (finished || updated) await refreshAll();
  } catch (error) {
    if (generation === state.generation) showNotice(error.message, true);
  } finally {
    state.polling = false;
    schedulePoll();
  }
}
function watchSync() {
  if (!("EventSource" in window)) return;
  const events = new EventSource("/api/sync/events");
  events.onopen = () => {
    state.syncStreamConnected = true;
    schedulePoll();
  };
  events.onmessage = (event) => {
    const status = JSON.parse(event.data);
    const previous = state.sync;
    const progress = status.progress;
    const finished = !status.pull_running && progress?.started_at &&
      !progress.started_at.startsWith("0001") &&
      (previous?.pull_running || previous?.progress?.started_at !== progress.started_at ||
        previous?.progress?.finished_at !== progress.finished_at);
    renderStatus({ ...state.status, ...status });
    if (finished) refreshAll();
  };
  events.onerror = () => {
    state.syncStreamConnected = false;
    // EventSource reconnects automatically; polling covers the gap and checks auth.
    pollStatus();
  };
}
async function sync(all = false, account = state.account) {
  if (state.syncPending || state.sync?.pull_running) return;
  state.syncPending = true;
  renderSyncControls();
  try {
    showNotice("");
    await api(all ? "/api/pull/trigger" : endpoint("/api/pull/trigger", account), {
      method: "POST",
      body: "{}",
    });
    await pollStatus();
  } catch (error) {
    if (error.status === 409 && error.code === "sync_in_progress") {
      // An automatic run can start between the latest status and this click.
      await refreshAll();
    } else {
      showNotice(error.message, true);
    }
  } finally {
    state.syncPending = false;
    renderSyncControls();
  }
}
function openConnect(id = state.account) {
  connectAccount = id;
  const account = state.accounts.find((a) => a.id === id);
  $("connectCopy").textContent =
    "Connect " + (account?.name || "this account") +
    " with its DEGIRO login. We’ll import its holdings after sign-in.";
  $("degiroDialogTitle").textContent = account?.connected ? "Reconnect DEGIRO" : "Connect DEGIRO";
  $("loginError").textContent = "";
  $("loginError").className = "error";
  $("degiroForm").reset();
  $("degiroDialog").showModal();
}
function selectAccount(id) {
  state.account = id;
  state.generation++;
  currentHoldings = [];
  state.transactions = [];
  state.history = [];
  state.valuation = null;
  state.previousSessionDayPL = null;
  state.returns = [];
  state.status = null;
  renderLastSync(state.accounts.find((account) => account.id === id));
  selectedCurrency = "";
  renderHoldings();
  renderTransactions();
  renderCurrencies();
  renderHistory();
  refreshAll();
}
document.addEventListener("click", (event) => {
  const b = event.target.closest("button");
  if (!b) return;
  if (b.dataset.view) showView(b.dataset.view);
  if (b.dataset.range) {
    state.range = b.dataset.range;
    document
      .querySelectorAll("[data-range]")
      .forEach((el) => el.setAttribute("aria-pressed", String(el === b)));
    renderHistory(false);
  }
  if (b.hasAttribute("data-connect"))
    openConnect(b.dataset.connect || state.account);
  if (b.dataset.selectAccount) {
    selectAccount(b.dataset.selectAccount);
    showView("overview");
  }
  if (b.dataset.close && !$("submitLoginButton").disabled)
    $(b.dataset.close).close();
});
$("accountSelect").addEventListener("change", (event) =>
  selectAccount(event.target.value),
);
$("chartCurrency").addEventListener("change", () => renderHistory());
$("pullButton").addEventListener("click", () => sync());
$("syncAllButton").addEventListener("click", () => sync(true));
$("refreshButton").addEventListener("click", refreshAll);
$("changelogButton").addEventListener("click", async () => {
  $("changelogDialog").showModal();
  $("changelogButton").disabled = true;
  $("changelogEntries").replaceChildren();
  $("changelogStatus").hidden = false;
  $("changelogStatus").textContent = "Loading release notes…";
  try {
    const changelog = await api("/changelog.json", { cache: "no-store" });
    for (const release of changelog.releases) {
      const article = document.createElement("article");
      article.className = "changelog-entry";
      const label = document.createElement("p");
      label.className = "eyebrow";
      label.textContent = release.version === changelog.version ? "This release" : "Earlier release";
      const heading = document.createElement("h3");
      heading.textContent = release.version;
      const list = document.createElement("ul");
      for (const change of release.changes) {
        const item = document.createElement("li");
        item.textContent = change;
        list.appendChild(item);
      }
      article.append(label, heading, list);
      $("changelogEntries").appendChild(article);
    }
    $("changelogStatus").textContent = "No release notes available for this build.";
    $("changelogStatus").hidden = changelog.releases.length > 0;
  } catch (error) {
    $("changelogEntries").replaceChildren();
    $("changelogStatus").textContent = error.status === 404
      ? "No release notes available for this build."
      : "Couldn’t load release notes. Close this window and try again.";
  } finally {
    $("changelogButton").disabled = false;
  }
});
$("addAccountButton").addEventListener("click", () => {
  $("accountError").className = "error";
  $("accountForm").reset();
  $("accountDialog").showModal();
});
$("accountForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const submit = event.target.querySelector("[type=submit]");
  submit.disabled = true;
  submit.textContent = "Adding account…";
  $("accountError").textContent = "";
  try {
    const account = await api("/api/accounts", {
      method: "POST",
      body: JSON.stringify({ name: new FormData(event.target).get("name") }),
    });
    $("accountDialog").close();
    state.account = account.id;
    await refreshAll();
    openConnect(account.id);
  } catch (error) {
    $("accountError").textContent = error.message;
    $("accountError").className = "error visible";
  } finally {
    submit.disabled = false;
    submit.textContent = "Continue to DEGIRO →";
  }
});
$("degiroForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("submitLoginButton");
  if (button.disabled) return;
  const account = connectAccount;
  const started = Date.now();
  let slow = false;
  const tick = () => {
    const seconds = Math.floor((Date.now() - started) / 1000);
    $("connectElapsed").textContent = `Elapsed · ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
    if (seconds >= 30 && !slow) {
      slow = true;
      $("connectWaitCopy").textContent = "Still waiting for DEGIRO to finish signing in. We’ll move on as soon as the connection is verified.";
    }
  };
  button.disabled = true;
  button.textContent = "Connecting…";
  $("loginError").textContent = "";
  $("loginError").className = "error";
  $("connectCredentials").disabled = true;
  $("connectCredentials").hidden = true;
  $("connectProgress").hidden = false;
  $("connectWaitCopy").textContent = "This can take a few minutes. Keep this window open.";
  $("degiroDialog").querySelectorAll("[data-close]").forEach(control => control.disabled = true);
  $("connectProgressTitle").focus();
  tick();
  const timer = setInterval(tick, 1000);
  const request = {
    username: $("degiroUsername").value,
    password: $("degiroPassword").value,
    seed: $("degiroSeed").value,
  };
  $("degiroPassword").value = "";
  $("degiroSeed").value = "";
  try {
    const body = JSON.stringify(request);
    request.password = request.seed = "";
    await api(endpoint("/api/degiro/reauthenticate", account), {
      method: "POST",
      body,
    });
    $("degiroDialog").close();
    state.account = account;
    showView("overview");
    showNotice("Account connected. Preparing the first sync…");
    await refreshAll();
    await sync(false, account);
    if (state.sync?.pull_running && !state.sync.progress?.accounts?.some(item => item.id === account))
      showNotice("Account connected. Another account is syncing; use Sync account when it finishes to import these holdings.");
    $("syncProgress").scrollIntoView({ block: "nearest" });
  } catch (error) {
    $("loginError").textContent = error.message;
    $("loginError").className = "error visible";
  } finally {
    clearInterval(timer);
    button.disabled = false;
    button.textContent = "Connect and sync →";
    $("connectCredentials").disabled = false;
    $("connectCredentials").hidden = false;
    $("connectProgress").hidden = true;
    $("degiroDialog").querySelectorAll("[data-close]").forEach(control => control.disabled = false);
    if ($("degiroDialog").open) $("degiroPassword").focus();
  }
});
$("degiroDialog").addEventListener("cancel", (e) => {
  if ($("submitLoginButton").disabled) e.preventDefault();
});
$("degiroDialog").addEventListener("close", () => $("degiroForm").reset());
for (const id of ["simInitial", "simMonthly", "simRate", "simYears"])
  $(id).addEventListener("input", renderSimulation);
$("returnInfo").addEventListener("click", () => {
  const hidden = !$("returnTooltip").hidden;
  $("returnTooltip").hidden = hidden;
  $("returnInfo").setAttribute("aria-expanded", String(!hidden));
});
document.addEventListener("pointerdown", (e) => {
  if (!$("returnHelp").contains(e.target)) {
    $("returnTooltip").hidden = true;
    $("returnInfo").setAttribute("aria-expanded", "false");
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    $("returnTooltip").hidden = true;
    $("returnInfo").setAttribute("aria-expanded", "false");
  }
});
let resizeTimer;
window.addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    renderHistory(false);
    renderSimulation();
  }, 100);
});

function renderReturnHelp() {
  const root = $("returnTooltip");
  root.replaceChildren();
  const paragraph = (text, strong = false) => {
    const p = document.createElement("p");
    if (strong) {
      const b = document.createElement("strong");
      b.textContent = text;
      p.appendChild(b);
    } else p.textContent = text;
    root.appendChild(p);
    return p;
  };
  const matched = holdingReturnReferences(currentHoldings, selectedCurrency);
  paragraph("Historical returns for your holdings", true);
  if (!matched.length)
    paragraph(
      "No holdings in the selected currency yet. Pull your portfolio to identify relevant funds.",
    );
  for (const { holding, reference } of matched) {
    paragraph(
      holding.symbol + " · holding currency " + (holding.currency || "unknown"),
      true,
    );
    const own = (state.returns || []).find(
      (r) =>
        r.identifier === String(holding.isin || "").toUpperCase() &&
        r.currency === holding.currency,
    );
    if (own) {
      paragraph("From DEGIRO closing prices (" + own.currency + ")", true);
      paragraph(priceReturnSummary(own), true);
      paragraph(
        "Weekly closes while you held it, through " +
          own.end_date +
          ". A price return: income counts only for accumulating funds, and your own buys and sells don't affect it.",
      );
    }
    if (!reference) {
      if (!own)
        paragraph(
          "No verified return reference is available for this holding. No proxy or portfolio average has been substituted.",
        );
      continue;
    }
    paragraph(reference.name + " · " + reference.isin);
    paragraph(
      "Annualized total returns in " +
        reference.currency +
        " through " +
        reference.asOf +
        ":",
    );
    paragraph(
      reference.annualized
        .map((r) => r.period + ": " + r.value.toFixed(2) + "% p.a.")
        .join(" · "),
      true,
    );
    paragraph(
      "Inception: " +
        reference.inception +
        ". NAV returns with dividends reinvested, net of fund fees; before your tax and dealing costs. Dated issuer snapshot, not live data.",
    );
    if (
      holding.currency === "EUR" &&
      reference.currency === "USD" &&
      reference.fxEnd
    ) {
      paragraph("EUR-adjusted annualized estimates", true);
      for (const r of reference.annualized) {
        paragraph(
          r.period +
            ": ≈" +
            annualizedEUR(r.value, r.years, r.fxStart, reference.fxEnd).toFixed(
              2,
            ) +
            "% p.a. in EUR",
          true,
        );
      }
      paragraph(
        "Calculated from rounded " +
          (reference.issuer || "issuer") +
          " USD returns and ECB reference FX, not an official EUR exchange-price return or your personal account return. NAV and ECB fixing times differ. These remain nominal, not inflation-adjusted.",
      );
      const details = document.createElement("details"),
        summary = document.createElement("summary");
      summary.textContent = "Conversion method and FX sources";
      details.appendChild(summary);
      const method = document.createElement("p");
      method.textContent =
        "EUR annualized = (1 + USD annualized) × (start FX / end FX)^(1 / years) − 1, with FX in USD per EUR. Since inception uses actual days / 365.25. Each period uses the last ECB reference rate published on or before its start date.";
      details.appendChild(method);
      for (const r of reference.annualized) {
        const p = document.createElement("p"),
          a = document.createElement("a");
        a.href = r.fxSource;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        a.textContent = r.fxDate + ": " + r.fxStart.toFixed(4) + " USD/EUR";
        p.appendChild(a);
        details.appendChild(p);
      }
      const p = document.createElement("p"),
        a = document.createElement("a");
      a.href = reference.fxEndSource;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.textContent =
        reference.asOf + ": " + reference.fxEnd.toFixed(4) + " USD/EUR";
      p.appendChild(a);
      details.appendChild(p);
      root.appendChild(details);
    } else if (holding.currency !== reference.currency)
      paragraph(
        "Currency mismatch: these are " +
          reference.currency +
          " returns, not your " +
          (holding.currency || "local-currency") +
          " return. Exchange-rate changes can materially alter the result.",
      );
    const p = paragraph("");
    const link = document.createElement("a");
    link.href = reference.source;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent =
      (reference.issuer || "Issuer") + " factsheet · " + reference.asOf;
    p.appendChild(link);
  }
  paragraph(
    "Use these as historical context, not a forecast. This field assumes a constant nominal return. Compare several scenarios; recent strong returns need not persist. Mixed holdings are not averaged.",
  );
}

showView("overview");
refreshAll();
watchSync();
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) pollStatus();
});
window.addEventListener("focus", () => pollStatus());

async function loadSureConnections() {
  const root = $("sureConnections");
  try {
    const connections = await api("/api/integrations/sure");
    root.replaceChildren();
    for (const connection of connections.filter((c) => !c.revoked_at)) {
      const row = document.createElement("div");
      row.className = "sure-connection";
      const description = document.createElement("p");
      const status = connection.claimed_at
        ? connection.last_used_at
          ? "Last read " + formatDate(connection.last_used_at, true)
          : "Connected · waiting for Sure’s first read"
        : new Date(connection.expires_at) < new Date()
          ? "Setup token expired"
          : "Waiting for setup in Sure";
      description.textContent = connection.name + " · " + status;
      const revoke = document.createElement("button");
      revoke.className = "text-button";
      revoke.textContent = "Revoke";
      revoke.addEventListener("click", async () => {
        revoke.disabled = true;
        try {
          await api(
            "/api/integrations/sure/" + encodeURIComponent(connection.id),
            { method: "DELETE", body: "{}" },
          );
          await loadSureConnections();
          showNotice(
            "Sure access revoked. Records already imported into Sure remain there.",
          );
        } catch (error) {
          showNotice(error.message, true);
          revoke.disabled = false;
        }
      });
      const actions = document.createElement("div");
      actions.className = "sure-connection-actions";
      const check = document.createElement("button");
      check.className = "text-button";
      check.textContent = "Check connection";
      check.addEventListener("click", () => openSureSetup(connection));
      actions.append(check, revoke);
      row.append(description, actions);
      root.appendChild(row);
    }
  } catch (error) {
    root.textContent = error.message;
  }
}
let sureConnectionID = null;
let sureDialogSession = 0;
let sureCheckRequest = 0;
let sureCreating = false;

function showSureStep(step) {
  document.querySelectorAll("[data-sure-step]").forEach((panel) => {
    panel.hidden = Number(panel.dataset.sureStep) !== step;
  });
  document.querySelectorAll("[data-sure-progress]").forEach((item) => {
    if (Number(item.dataset.sureProgress) === step)
      item.setAttribute("aria-current", "step");
    else item.removeAttribute("aria-current");
    item.classList.toggle("complete", Number(item.dataset.sureProgress) < step);
  });
  $("sureDialog").scrollTop = 0;
  document.querySelector(`[data-sure-step="${step}"] .sure-step-title`).focus();
}

function updateSureMethod() {
  const docker = $("sureForm").elements.sureMethod.value === "docker";
  $("sureDockerSetup").hidden = !docker;
  $("sureHTTPSSetup").hidden = docker;
  $("sureBaseURL").value = docker
    ? "http://degitide-sure:8391"
    : location.protocol === "https:"
      ? location.origin
      : "";
  $("sureBaseURL").placeholder = docker
    ? "http://degitide-sure:8391"
    : "https://portfolio.example.com";
  $("sureAddressHelp").textContent = docker
    ? "Use this address after connecting both apps to the private Docker network below."
    : "Enter Degitide’s HTTPS address, not the Sure URL or a temporary laptop tunnel.";
}

function openSureSetup(connection = null) {
  sureDialogSession++;
  sureConnectionID = connection?.id || null;
  $("sureForm").reset();
  $("sureToken").value = "";
  $("copySureToken").textContent = "Copy token";
  $("copySureToken").disabled = false;
  $("sureError").hidden = true;
  $("sureDialog")
    .querySelectorAll("details")
    .forEach((detail) => (detail.open = false));
  const select = $("sureAccount");
  select.replaceChildren(new Option("All current accounts", ""));
  for (const account of state.accounts)
    select.appendChild(new Option(account.name, account.id));
  $("sureForm").elements.sureMethod.value =
    location.protocol === "https:" ? "https" : "docker";
  updateSureMethod();
  if (connection) {
    if (connection.base_url) {
      $("sureForm").elements.sureMethod.value = connection.base_url.startsWith(
        "https:",
      )
        ? "https"
        : "docker";
      updateSureMethod();
      $("sureBaseURL").value = connection.base_url;
    }
    if (connection.account_ids?.length) {
      const previous = new Option(
        "Previously shared accounts (" + connection.account_ids.length + ")",
        "",
        false,
        true,
      );
      previous.dataset.accountIds = JSON.stringify(connection.account_ids);
      select.appendChild(previous);
    }
  }
  $("sureDialog").showModal();
  if (connection) {
    showSureStep(3);
    checkSureConnection();
  } else showSureStep(1);
}

$("connectSureButton").addEventListener("click", () => openSureSetup());
$("sureForm")
  .querySelectorAll('[name="sureMethod"]')
  .forEach((input) => input.addEventListener("change", updateSureMethod));
$("closeSureDialog").addEventListener("click", () => $("sureDialog").close());
$("sureDialog").addEventListener("cancel", (event) => {
  if (sureCreating) event.preventDefault();
});
$("sureDialog").addEventListener("close", () => {
  sureDialogSession++;
  sureConnectionID = null;
  $("sureToken").value = "";
  loadSureConnections();
});
$("sureForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (sureCreating) return;
  sureCreating = true;
  $("sureError").hidden = true;
  $("createSureToken").textContent = "Creating token…";
  $("closeSureDialog").disabled = true;
  const body = JSON.stringify({
    base_url: $("sureBaseURL").value.trim(),
    account_ids: $("sureAccount").selectedOptions[0]?.dataset.accountIds
      ? JSON.parse($("sureAccount").selectedOptions[0].dataset.accountIds)
      : $("sureAccount").value
        ? [$("sureAccount").value]
        : [],
  });
  $("sureForm")
    .querySelectorAll("input, select, button")
    .forEach((input) => (input.disabled = true));
  try {
    const result = await api("/api/integrations/sure", {
      method: "POST",
      body,
    });
    sureConnectionID = result.id;
    $("sureToken").value = result.setup_token;
    $("copySureToken").disabled = false;
    $("copySureToken").textContent = "Copy token";
    $("sureTokenExpiry").textContent =
      "Use once, before " +
      new Intl.DateTimeFormat(undefined, {
        hour: "numeric",
        minute: "2-digit",
      }).format(new Date(result.expires_at)) +
      ". Treat it like a password.";
    showSureStep(2);
    loadSureConnections();
  } catch (error) {
    $("sureError").textContent = error.message;
    $("sureError").hidden = false;
  } finally {
    sureCreating = false;
    $("closeSureDialog").disabled = false;
    $("sureForm")
      .querySelectorAll("input, select, button")
      .forEach((input) => (input.disabled = false));
    $("createSureToken").textContent = "Create connection token →";
  }
});
$("copySureToken").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText($("sureToken").value);
    $("copySureToken").textContent = "Copied";
  } catch {
    $("sureToken").focus();
    $("sureToken").select();
    $("copySureToken").textContent = "Select and copy token";
  }
});

async function checkSureConnection() {
  const session = sureDialogSession;
  const request = ++sureCheckRequest;
  const isCurrent = () =>
    session === sureDialogSession && request === sureCheckRequest;
  $("sureCheckTitle").textContent = "Checking…";
  $("sureCheckDetail").textContent = "";
  $("sureCheck").classList.remove("connected");
  $("refreshSureConnection").disabled = true;
  $("refreshSureConnection").hidden = false;
  $("finishSureSetup").hidden = true;
  $("restartSureSetup").hidden = true;
  $("backSureToken").hidden = !$("sureToken").value;
  try {
    const connections = await api("/api/integrations/sure");
    if (!isCurrent()) return;
    const connection = connections.find((item) => item.id === sureConnectionID);
    if (connection?.claimed_at) {
      $("copySureToken").disabled = true;
      $("sureTokenExpiry").textContent =
        "Token already accepted. Finish linking your accounts in Sure.";
    }
    if (
      !connection ||
      connection.revoked_at ||
      (!connection.claimed_at && new Date(connection.expires_at) <= new Date())
    ) {
      $("sureCheckTitle").textContent = connection?.revoked_at
        ? "This connection was revoked"
        : "This token is no longer available";
      $("sureCheckDetail").textContent =
        "Create a new token, then use it in Sure. If you already linked accounts, reconnect them instead of creating duplicates.";
      $("restartSureSetup").hidden = false;
      $("refreshSureConnection").hidden = true;
      $("backSureToken").hidden = true;
      $("sureToken").value = "";
    } else if (connection.last_used_at) {
      $("sureCheckTitle").textContent = "Sure reached Degitide";
      $("sureCheckDetail").textContent =
        "Last read: " +
        formatDate(connection.last_used_at, true) +
        ". Open Sure to confirm your accounts and holdings look right.";
      $("sureCheck").classList.add("connected");
      $("finishSureSetup").hidden = false;
    } else if (connection.claimed_at) {
      $("sureCheckTitle").textContent =
        "Token accepted. Waiting for the first read.";
      $("sureCheckDetail").textContent =
        "Finish linking your accounts and run a sync in Sure, then check again.";
    } else {
      $("sureCheckTitle").textContent = "Waiting for Sure to accept the token";
      $("sureCheckDetail").textContent = $("sureToken").value
        ? "Paste the token into Sure → Add account → SimpleFIN, then finish linking your accounts."
        : "Paste your copied token into Sure → Add account → SimpleFIN. If you closed setup without copying it, revoke this pending connection and create a new one.";
    }
  } catch (error) {
    if (!isCurrent()) return;
    $("sureCheckTitle").textContent = "Couldn’t check the connection";
    $("sureCheckDetail").textContent = error.message + " Try checking again.";
  } finally {
    if (isCurrent()) $("refreshSureConnection").disabled = false;
  }
}
$("checkSureConnection").addEventListener("click", () => {
  showSureStep(3);
  checkSureConnection();
});
$("refreshSureConnection").addEventListener("click", checkSureConnection);
$("backSureToken").addEventListener("click", () => showSureStep(2));
$("finishSureSetup").addEventListener("click", () => {
  $("sureDialog").close();
});
$("restartSureSetup").addEventListener("click", () => {
  $("sureToken").value = "";
  sureConnectionID = null;
  showSureStep(1);
});

let scheduleDraft = null;
let scheduleAccount = "";
let savingSchedule = false;
function updateScheduleFields() {
  const market = $("scheduleMode").value === "market";
  const custom = $("scheduleMode").value === "cron";
  $("marketSettingsFields").hidden = !market;
  $("marketSettingsFields").disabled = !market;
  $("customScheduleField").hidden = !custom;
  $("scheduleCron").disabled = !custom;
  $("scheduleCron").required = custom;
  $("schedulePausedCopy").hidden = $("scheduleMode").value !== "manual";
  const customSafety = $("customSafety").checked;
  $("dailySafetyFields").hidden = customSafety;
  $("scheduleSafetyTime").disabled = customSafety;
  $("scheduleTimezone").disabled = customSafety;
  $("customSafetyField").hidden = !customSafety;
  $("scheduleMaintenance").disabled = !customSafety;
  $("scheduleMaintenance").required = customSafety;
}
$("scheduleMode").addEventListener("change", updateScheduleFields);
$("customSafety").addEventListener("change", updateScheduleFields);
$("scheduleSettingsButton").addEventListener("click", () => openSchedule(state.account));
$("workspaceScheduleButton").addEventListener("click", () => openSchedule(""));
function schedulePath(account) {
  return account ? endpoint("/api/schedule", account) : "/api/schedule";
}
async function openSchedule(account) {
  $("scheduleSettingsButton").disabled = $("workspaceScheduleButton").disabled = true;
  try {
    const view = await api(schedulePath(account));
    if (account && account !== state.account) return;
    scheduleAccount = account;
    const name = state.accounts.find(a => a.id === account)?.name || account;
    $("scheduleTitle").textContent = account ? "Sync settings · " + name : "Workspace sync defaults";
    scheduleDraft = view.settings;
    $("scheduleMode").value = scheduleDraft.mode;
    $("scheduleInterval").value = scheduleDraft.interval_minutes;
    $("scheduleCloseDelay").value = scheduleDraft.close_delay_minutes;
    $("scheduleCron").value = scheduleDraft.cron;
    $("scheduleMaintenance").value = scheduleDraft.maintenance;
    const daily = /^CRON_TZ=(\S+) (\d{1,2}) (\d{1,2}) \* \* \*$/.exec(scheduleDraft.maintenance);
    $("scheduleTimezone").value = daily?.[1] || "Europe/Dublin";
    $("scheduleSafetyTime").value = daily ? daily[3].padStart(2, "0") + ":" + daily[2].padStart(2, "0") : "02:00";
    $("customSafety").checked = !daily;
    $("advancedSafety").open = !daily;
    if (account) {
      $("scheduleSource").textContent = view.source === "account"
        ? name + " uses its own schedule."
        : name + " follows the workspace schedule. Save to give this account its own schedule.";
      $("resetSchedule").textContent = "Use workspace schedule";
      $("resetSchedule").disabled = view.source !== "account";
    } else {
      $("scheduleSource").textContent = view.source === "dashboard"
        ? "Accounts following the workspace schedule use these saved dashboard settings."
        : "Accounts following the workspace schedule use the deployment defaults.";
      $("resetSchedule").textContent = "Restore deployment defaults";
      $("resetSchedule").disabled = view.source !== "dashboard";
    }
    $("scheduleError").textContent = "";
    updateScheduleFields();
    $("scheduleDialog").showModal();
  } catch (error) {
    showNotice(error.message, true);
  } finally {
    $("scheduleSettingsButton").disabled = $("workspaceScheduleButton").disabled = false;
  }
}
async function saveSchedule(reset = false) {
  if (savingSchedule) return;
  const settings = { ...scheduleDraft, mode: $("scheduleMode").value };
  if (settings.mode === "market") {
    settings.interval_minutes = Number($("scheduleInterval").value);
    settings.close_delay_minutes = Number($("scheduleCloseDelay").value);
    const [hour, minute] = $("scheduleSafetyTime").value.split(":");
    settings.maintenance = $("customSafety").checked ? $("scheduleMaintenance").value.trim()
      : `CRON_TZ=${$("scheduleTimezone").value.trim()} ${Number(minute)} ${Number(hour)} * * *`;
  }
  if (settings.mode === "cron") settings.cron = $("scheduleCron").value.trim();
  savingSchedule = true;
  const controls = [...$("scheduleForm").querySelectorAll("button, input, select")];
  const disabled = controls.map(control => control.disabled);
  controls.forEach(control => control.disabled = true);
  $("scheduleError").textContent = "";
  try {
    await api(schedulePath(scheduleAccount), { method: reset ? "DELETE" : "PUT", body: reset ? "{}" : JSON.stringify(settings) });
    await refreshAll();
    $("scheduleDialog").close();
    const name = state.accounts.find(a => a.id === scheduleAccount)?.name || scheduleAccount;
    if (scheduleAccount) {
      showNotice(reset ? name + " now follows the workspace schedule." : "Sync settings saved for " + name + ".");
    } else {
      showNotice(reset ? "Deployment sync defaults restored." : "Workspace sync defaults saved.");
    }
  } catch (error) {
    $("scheduleError").textContent = error.message;
    $("scheduleError").className = "error visible";
  } finally {
    savingSchedule = false;
    controls.forEach((control, index) => control.disabled = disabled[index]);
  }
}
$("scheduleForm").addEventListener("submit", event => { event.preventDefault(); saveSchedule(); });
$("resetSchedule").addEventListener("click", () => saveSchedule(true));
$("scheduleDialog").addEventListener("cancel", event => { if (savingSchedule) event.preventDefault(); });
