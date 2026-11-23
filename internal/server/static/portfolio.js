/* Pure projection math is also exercised under Node. Contributions occur at month end. */
// Use the same displayed precision for money and its direction. Broker floating
// point residues must never turn a displayed zero into a red loss or -€0.00.
const moneyPrecision = new Intl.NumberFormat("en", {useGrouping: false, maximumFractionDigits: 2});
function roundedMoney(value) {
  if (value == null || !Number.isFinite(Number(value))) return null;
  return Number(moneyPrecision.format(Number(value))) || 0;
}
function moneyDirection(value) {
  const amount = roundedMoney(value);
  return amount == null ? "unavailable" : amount > 0 ? "gain" : amount < 0 ? "loss" : "neutral";
}
function projectPortfolio(initial, monthly, annual, years) {
  if (![initial, monthly, annual, years].every(Number.isFinite) || initial < 0 || monthly < 0 || annual <= -100 || years < 1 || years > 50) throw Error("Invalid projection assumptions");
  const rate = Math.pow(1 + annual / 100, 1 / 12) - 1;
  const points = [{x: 0, value: initial, contributed: initial}];
  let value = initial;
  for (let month = 1; month <= years * 12; month++) {
    value = value * (1 + rate) + monthly;
    if (month % 12 === 0) points.push({x: month / 12, value, contributed: initial + monthly * month});
  }
  return points;
}
if (typeof module !== "undefined") module.exports = {projectPortfolio, roundedMoney, moneyDirection};

// Dated issuer snapshots, not live quotes, generated weekly into
// fund-returns.js by scripts/update-fund-returns.py. Keep their as-of dates visible.
function fundReferences() {
  if (typeof fundReturnReferences !== "undefined") return fundReturnReferences;
  return require("./fund-returns.js").fundReturnReferences;
}
function holdingReturnReferences(holdings, currency) {
  return holdings.filter(h => h.currency === currency && h.quantity !== 0).map(holding => ({
    holding,
    reference: fundReferences().find(f => f.symbols.includes(String(holding.symbol || "").trim().toUpperCase())) || null
  }));
}
if (typeof module !== "undefined") module.exports.holdingReturnReferences = holdingReturnReferences;

// FX quotes are USD per EUR. Convert total growth before annualizing.
function annualizedEUR(usdPercent, years, startUSDPerEUR, endUSDPerEUR) {
  if (![usdPercent, years, startUSDPerEUR, endUSDPerEUR].every(Number.isFinite) ||
      usdPercent <= -100 || years <= 0 || startUSDPerEUR <= 0 || endUSDPerEUR <= 0) {
    throw Error("Invalid annualized return or exchange rate");
  }
  return ((1 + usdPercent / 100) * Math.pow(startUSDPerEUR / endUSDPerEUR, 1 / years) - 1) * 100;
}
if (typeof module !== "undefined") module.exports.annualizedEUR = annualizedEUR;

// ETF issuers. `logo` means static/logos/<id>.svg is bundled (sanitized,
// public-domain wordmarks from Wikimedia Commons); others get a text badge.
// DEGIRO product names start with the issuer's brand; the tickers cover rows
// without a name, such as transactions in products no longer held.
const fundIssuers = [
  {id: "vanguard", label: "Vanguard", logo: true, names: /^VANGUARD\b/i,
    symbols: ["VWCE", "VWRL", "VWRA", "VWRP", "VUSA", "VUAA", "VEVE", "VFEM"]},
  {id: "ishares", label: "iShares", logo: true, names: /^ISHARES\b/i,
    symbols: ["EUNL", "SWDA", "IWDA", "CSPX", "SXR8", "IUSQ", "IS3N", "EIMI"]},
  {id: "amundi", label: "Amundi", logo: true, names: /^(AMUNDI|LYXOR)\b/i, symbols: []},
  {id: "vaneck", label: "VanEck", logo: true, names: /^VANECK\b/i, symbols: []},
  {id: "xtrackers", label: "Xtrackers", logo: false, names: /^(XTRACKERS|DB X-TRACKERS|X-TRACKERS)\b/i, symbols: []},
  {id: "spdr", label: "SPDR", logo: false, names: /^SPDR\b/i, symbols: []},
  {id: "invesco", label: "Invesco", logo: false, names: /^INVESCO\b/i, symbols: []},
  {id: "hsbc", label: "HSBC", logo: false, names: /^HSBC\b/i, symbols: []},
  {id: "wisdomtree", label: "WisdomTree", logo: false, names: /^WISDOMTREE\b/i, symbols: []},
];
function fundIssuer(item) {
  const name = String(item?.name || "").trim();
  const symbol = String(item?.symbol || "").trim().toUpperCase();
  return (name && fundIssuers.find(i => i.names.test(name))) ||
    fundIssuers.find(i => i.symbols.includes(symbol)) || null;
}
if (typeof module !== "undefined") module.exports.fundIssuer = fundIssuer;

// Average net buys per month in one currency: gross BUY minus SELL
// consideration over the last 12 months, never below zero. When the loaded
// transactions start less than a year ago (a new account, or the dashboard's
// latest-100 page), it averages over the months they cover instead.
function averageMonthlyContribution(transactions, currency, now = Date.now()) {
  const month = 365.25 * 86400000 / 12;
  const dated = (transactions || [])
    .map(t => ({t, at: Date.parse(String(t.date || "").slice(0, 10))}))
    .filter(({at}) => Number.isFinite(at) && at <= now);
  if (!dated.length) return null;
  const yearAgo = now - 12 * month;
  const oldest = Math.min(...dated.map(({at}) => at));
  const months = oldest > yearAgo ? Math.max(1, Math.round((now - oldest) / month)) : 12;
  let net = 0, trades = 0;
  for (const {t, at} of dated) {
    if (at < yearAgo || t.currency !== currency) continue;
    const amount = Math.abs(Number(t.quantity) * Number(t.price));
    const type = String(t.type || "").toUpperCase();
    if (!Number.isFinite(amount) || (type !== "BUY" && type !== "SELL")) continue;
    net += type === "BUY" ? amount : -amount;
    trades++;
  }
  if (!trades) return null;
  return {monthly: Math.max(0, net / months), months, trades};
}
if (typeof module !== "undefined") module.exports.averageMonthlyContribution = averageMonthlyContribution;

// One line per period of a /api/portfolio/returns entry: annualized when the
// period spans a year or more, otherwise the plain total change.
function priceReturnSummary(entry) {
  return (entry?.periods || []).map(p => {
    const label = p.label === "Since first held" ? "Since " + p.start_date : p.label;
    if (Number.isFinite(p.annualized)) return label + ": " + p.annualized.toFixed(2) + "% p.a.";
    return label + ": " + (p.total >= 0 ? "+" : "") + Number(p.total).toFixed(2) + "% total";
  }).join(" · ");
}
if (typeof module !== "undefined") module.exports.priceReturnSummary = priceReturnSummary;
