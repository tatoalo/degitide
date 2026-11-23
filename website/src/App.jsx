import { useEffect, useRef, useState } from "react";
import { Architecture } from "./Architecture.jsx";
import { AgentSetup } from "./AgentSetup.jsx";
import {
  ArrowUpRight,
  Check,
  Copy,
  GithubLogo,
  List,
  Robot,
  X,
  ArrowClockwise,
  TerminalWindow,
} from "@phosphor-icons/react";
import "@fontsource/ibm-plex-mono/400.css";

const repository = "https://github.com/tatoalo/degitide";
const money = (value) =>
  new Intl.NumberFormat("en-IE", { style: "currency", currency: "EUR" }).format(
    value,
  );
const examples = {
  personal: {
    label: "Personal",
    value: 124280.5,
    invested: 94000,
    positions: [
      ["BLRP", "Blorp & Bloop", 51400],
      ["WIBL", "Wibble Wobble", 26400],
      ["ZORB", "Zorb Industries", 17800],
      ["FLUM", "Flumflam 500", 28680.5],
    ],
  },
  savings: {
    label: "Long-term savings",
    value: 38640,
    invested: 32000,
    positions: [
      ["BLRP", "Blorp & Bloop", 27200],
      ["FLUM", "Flumflam 500", 11440],
    ],
  },
};
const stages = [
  "Checking connection",
  "Fetching transactions",
  "Fetching holdings",
  "Saving snapshot",
  "Both accounts synced",
];
const months = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
];

function Brand() {
  return (
    <a className="brand" href="#top" aria-label="Degitide home">
      <img src="/assets/degitide-mark.svg" alt="" />
      degitide
    </a>
  );
}

function TideDiagram() {
  const tide = Array.from({ length: 19 }, (_, row) =>
    Array.from({ length: 61 }, (_, col) => {
      const crest = 8 + Math.sin(col / 6) * 3 + Math.cos(col / 13) * 2;
      if (row < crest) return " ";
      if (row < crest + 1) return "▄";
      return ["░", "▒", "▓", "█"][Math.min(3, Math.floor((row - crest) / 3))];
    }).join(""),
  ).join("\n");
  return (
    <figure
      className="tide-diagram"
      aria-label="An illustrated tide of portfolio records, flowing from DEGIRO to your own server."
    >
      <div className="diagram-caption">
        <span>DEGIRO</span>
        <span className="flow-line">──────────→</span>
        <span>YOUR SERVER</span>
      </div>
      <div className="tide-field" aria-hidden="true">
        <span className="tide-coordinate">fig. 01 / the tide keeps moving</span>
        <pre>{tide}</pre>
        <span className="tide-marker">Saved on your server</span>
      </div>
      <figcaption>
        <span>01 READ</span>
        <span>02 RECORD</span>
        <span>03 REVISIT</span>
      </figcaption>
    </figure>
  );
}
function Command({ children }) {
  const [status, setStatus] = useState("Copy");
  const code = useRef(null);
  async function copy() {
    try {
      await navigator.clipboard.writeText(children);
      setStatus("Copied");
    } catch {
      const range = document.createRange();
      range.selectNodeContents(code.current);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      setStatus("Selected");
    }
  }
  return (
    <div className="command">
      <code ref={code}>{children}</code>
      <button onClick={copy} aria-label={`Copy command: ${children}`}>
        {status === "Copied" ? <Check size={17} /> : <Copy size={17} />}
        <span role="status">{status}</span>
      </button>
    </div>
  );
}
function DemoChart({ sample }) {
  const [active, setActive] = useState(null);
  const valueShape = [
    0.18, 0.22, 0.24, 0.23, 0.35, 0.39, 0.47, 0.57, 0.63, 0.82, 1,
  ];
  const points = valueShape.map((factor, index) => ({
    value: sample.value * factor,
    invested: sample.invested * (0.2 + index * 0.08),
  }));
  const x = (index) => 36 + index * 44.8;
  const y = (value) => 202 - (value / (sample.value * 1.12)) * 180;
  const line = (key) =>
    points.map((p, i) => `${i ? "L" : "M"}${x(i)},${y(p[key])}`).join(" ");
  const current = active === null ? null : points[active];
  function inspect(event) {
    const rect = event.currentTarget.getBoundingClientRect();
    const localX = ((event.clientX - rect.left) / rect.width) * 500;
    setActive(Math.max(0, Math.min(10, Math.round((localX - 36) / 44.8))));
  }
  return (
    <div className="demo-chart-wrap">
      <div className="chart-title">
        <span>Value history</span>
        <span className="chart-period">2026</span>
      </div>
      <svg
        className="portfolio-chart"
        viewBox="0 0 500 234"
        role="group"
        aria-roledescription="interactive chart"
        tabIndex="0"
        aria-label="Sample portfolio value and net invested. Use the arrow keys to inspect values."
        onPointerMove={inspect}
        onPointerDown={inspect}
        onPointerLeave={() => setActive(null)}
        onFocus={() => setActive(10)}
        onBlur={() => setActive(null)}
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            setActive(null);
            return;
          }
          const target = {
            ArrowLeft: Math.max(0, (active ?? 10) - 1),
            ArrowRight: Math.min(10, (active ?? 0) + 1),
            Home: 0,
            End: 10,
          }[e.key];
          if (target !== undefined) {
            e.preventDefault();
            setActive(target);
          }
        }}
      >
        {[0.0, 0.5, 1].map((factor) => (
          <g key={factor}>
            <line
              x1="36"
              x2="484"
              y1={y(sample.value * factor)}
              y2={y(sample.value * factor)}
              stroke="var(--line)"
              strokeWidth=".8"
            />
            <text x="26" y={y(sample.value * factor) + 3} textAnchor="end">
              {Math.round((sample.value * factor) / 1000)}k
            </text>
          </g>
        ))}
        <path
          d={line("invested")}
          fill="none"
          stroke="#91a79d"
          strokeWidth="1.8"
          strokeDasharray="5 5"
        />
        <path
          d={line("value")}
          fill="none"
          stroke="var(--accent)"
          strokeWidth="2.5"
          strokeLinejoin="round"
        />
        <text x="36" y="230">
          Jan 2026
        </text>
        <text x="484" y="230" textAnchor="end">
          Nov 2026
        </text>
        {current && (
          <g>
            <line
              x1={x(active)}
              x2={x(active)}
              y1="12"
              y2="202"
              stroke="var(--accent)"
              strokeOpacity=".4"
              strokeDasharray="3 4"
            />
            <circle
              cx={x(active)}
              cy={y(current.value)}
              r="5"
              fill="var(--accent)"
              stroke="white"
              strokeWidth="2"
            />
            <circle
              cx={x(active)}
              cy={y(current.invested)}
              r="4"
              fill="#91a79d"
              stroke="white"
              strokeWidth="2"
            />
          </g>
        )}
      </svg>
      {current && (
        <div
          className={`demo-tooltip ${active < 5 ? "right" : "left"}`}
          role="status"
        >
          <p>{months[active]} 2026</p>
          <div>
            Portfolio value <b>{money(current.value)}</b>
          </div>
          <div>
            Net invested <b>{money(current.invested)}</b>
          </div>
          <div>
            Trading P&amp;L{" "}
            <b
              className={
                current.value >= current.invested ? "positive" : "negative"
              }
            >
              {current.value >= current.invested ? "+" : ""}
              {money(current.value - current.invested)}
            </b>
          </div>
        </div>
      )}
      <div className="chart-key">
        <span>
          <i />
          Portfolio value
        </span>
        <span>
          <i className="dashed" />
          Net invested
        </span>
      </div>
    </div>
  );
}
function ProductPreview() {
  const [account, setAccount] = useState("personal"),
    [stage, setStage] = useState(-1);
  const timer = useRef(null);
  useEffect(() => () => clearInterval(timer.current), []);
  function sync() {
    clearInterval(timer.current);
    setStage(0);
    let next = 0;
    timer.current = setInterval(() => {
      next += 1;
      setStage(next);
      if (next >= 4) clearInterval(timer.current);
    }, 650);
  }
  const sample = examples[account];
  return (
    <div className="product-preview" aria-label="Interactive portfolio example">
      <div className="preview-bar">
        <span className="mini-brand">
          <img src="/assets/degitide-mark.svg" alt="" />
          degitide
        </span>
        <span className="preview-tab">Overview</span>
        <span className="sample-label">
          <i />
          Illustrative data
        </span>
      </div>
      <div className="preview-body">
        <div className="preview-controls">
          <div
            className="account-tabs"
            role="group"
            aria-label="Sample account"
          >
            {Object.entries(examples).map(([id, value]) => (
              <button
                key={id}
                aria-pressed={account === id}
                onClick={() => setAccount(id)}
              >
                {value.label}
              </button>
            ))}
          </div>
          <button
            className="demo-sync"
            onClick={sync}
            disabled={stage >= 0 && stage < 4}
          >
            <ArrowClockwise size={14} />
            {stage >= 0 && stage < 4 ? "Syncing…" : "Try a sync"}
          </button>
        </div>
        {stage >= 0 && (
          <div className="demo-progress">
            <div>
              <span role="status">{stages[stage]}</span>
              <span>{stage * 25}%</span>
            </div>
            <progress
              max="100"
              value={stage * 25}
              aria-label="Example sync progress"
            />
            <small>Demo only · no broker connection</small>
          </div>
        )}
        <div className="preview-main">
          <div className="preview-summary">
            <p className="eyebrow">Recorded holdings value</p>
            <div className="preview-value">{money(sample.value)}</div>
            <p className="subtle">{sample.positions.length} positions · EUR</p>
            <dl className="preview-performance">
              <div>
                <dt>Net invested</dt>
                <dd>{money(sample.invested)}</dd>
              </div>
              <div>
                <dt>Trading P&amp;L</dt>
                <dd className="positive">
                  +{money(sample.value - sample.invested)}
                </dd>
              </div>
            </dl>
            <DemoChart key={account} sample={sample} />
          </div>
          <div className="preview-side">
            <div className="preview-positions">
              <div className="demo-section-heading">
                <h3>Your holdings</h3>
                <span>{sample.positions.length} positions</span>
              </div>
              {sample.positions.map(([symbol, name, value]) => (
                <div className="position" key={symbol}>
                  <span>
                    <b>{symbol}</b>
                    <small>{name}</small>
                  </span>
                  <span>{money(value)}</span>
                </div>
              ))}
            </div>
            <div className="preview-allocation">
              <div className="demo-section-heading">
                <h3>Allocation</h3>
                <span>EUR holdings</span>
              </div>
              <div className="allocation-bar">
                {sample.positions.map(([symbol, , value], index) => (
                  <i
                    key={symbol}
                    style={{
                      width: (value / sample.value) * 100 + "%",
                      opacity: 1 - index * 0.19,
                    }}
                  />
                ))}
              </div>
              <div className="allocation-labels">
                {sample.positions.map(([symbol, , value]) => (
                  <span key={symbol}>
                    {symbol}
                    <b>{((value / sample.value) * 100).toFixed(1)}%</b>
                  </span>
                ))}
              </div>
            </div>
            <div className="demo-saved">
              <Check size={15} />
              <span>
                Saved on your server.
                <br />
                <small>Ready when you come back.</small>
              </span>
            </div>
          </div>
        </div>
      </div>
      <div className="preview-footer">
        <span>Purchases − sales = net invested</span>
        <span>P&amp;L before fees &amp; taxes. Excludes dividends.</span>
      </div>
    </div>
  );
}
const questions = [
  [
    "What is Degitide?",
    "A self-hosted companion for your DEGIRO accounts. It saves your holdings and transactions, syncs in the background, and gives you a dashboard and CLI to browse them.",
  ],
  [
    "What does the P&L chart measure?",
    "Portfolio value compared with cumulative purchases minus sale proceeds in the same currency. Trading P&L includes realized and unrealized trading gains, before fees and taxes. It excludes cash and dividends. Transfers or missing trades can distort it. A full saved transaction history is needed.",
  ],
  [
    "Does my browser need to stay open?",
    "No. Scheduled syncs run on your server. You can also connect accounts, start a sync and check progress entirely from the CLI.",
  ],
  [
    "Where does my data live?",
    "On the machine where you run Degitide. Each DEGIRO account keeps separate credentials and records. You choose where to host it and how to back it up.",
  ],
  [
    "Can it place trades?",
    "No. Degitide has no trading controls. It reads and saves your portfolio data. It is an independent project and is not affiliated with DEGIRO.",
  ],
];
export function App() {
  const [menu, setMenu] = useState(false);
  return (
    <div className="site" id="top">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="site-header">
        <Brand />
        <button
          className="menu-toggle"
          onClick={() => setMenu(!menu)}
          aria-controls="navigation"
          aria-expanded={menu}
          aria-label={menu ? "Close menu" : "Open menu"}
        >
          {menu ? <X size={22} /> : <List size={22} />}
        </button>
        <nav
          id="navigation"
          className={menu ? "navigation open" : "navigation"}
          aria-label="Main navigation"
        >
          <a href="#portfolio" onClick={() => setMenu(false)}>
            <span>[01]</span> Product
          </a>
          <a href="#architecture" onClick={() => setMenu(false)}>
            <span>[02]</span> Architecture
          </a>
          <a href="#setup" onClick={() => setMenu(false)}>
            <span>[03]</span> Get started
          </a>
          <a href="#questions" onClick={() => setMenu(false)}>
            <span>[04]</span> Questions
          </a>
          <a
            className="repo-link"
            href={repository}
            target="_blank"
            rel="noreferrer"
          >
            GitHub <GithubLogo size={18} />
          </a>
        </nav>
      </header>
      <main id="main">
        <section className="hero section-frame">
          <div className="hero-copy">
            <div className="section-index">
              <i className="status-dot" /> THE SELF-HOSTED PORTFOLIO COMPANION
            </div>
            <h1>
              Your portfolio.
              <br />
              <span>You keep the keys.</span>
            </h1>
            <div className="hero-bottom">
              <p>
                Your DEGIRO accounts, holdings and history, on your own server.
                A clear view of what you own. A copy that stays yours.
              </p>
              <AgentSetup />
              <p className="hero-access">
                <a href={repository}>Open source on GitHub</a>.
              </p>
            </div>
          </div>
          <TideDiagram />
        </section>
        <div className="principles-strip" aria-label="Product principles">
          <span>
            <b>01 /</b> Multiple accounts
          </span>
          <span>
            <b>02 /</b> Scheduled syncs
          </span>
          <span>
            <b>03 /</b> Export your history
          </span>
          <span>
            <b>04 /</b> No hosted relay
          </span>
        </div>
        <section id="portfolio" className="product-section section-frame">
          <div className="section-caption">
            <span>[01] THE OBSERVATORY</span>
            <span>Try the chart. Switch accounts. Run a sample sync.</span>
          </div>
          <ProductPreview />
          <div className="product-notes">
            <p>
              <b>Value has context.</b> Compare your holdings with net invested,
              and inspect the trading P&amp;L at each point.
            </p>
            <p>
              <b>Every account has its place.</b> Switch accounts without mixing
              holdings, history or currencies.
            </p>
          </div>
        </section>
        <section
          className="capabilities section-frame"
          aria-labelledby="capabilities-title"
        >
          <div className="section-intro">
            <p className="section-index">A SMALL TOOL. THE WHOLE ROUTINE.</p>
            <h2 id="capabilities-title">
              Check in.
              <br />
              Carry on.
            </h2>
            <p>
              Let your server handle the repeat work. Open the dashboard when
              you want the detail.
            </p>
            <pre
              className="routine-diagram"
              aria-hidden="true"
            >{`┌─ YOUR SERVER ───────────────┐
│                            │
│  ◷  schedule               │
│  │                         │
│  ├── read accounts         │
│  ├── save a snapshot       │
│  └── ready when you are    │
│                            │
└────────────────────────────┘`}</pre>
          </div>
          <div className="feature-list">
            {[
              [
                "01",
                "Sync without the tab.",
                "Scheduled pulls keep running when your browser is closed. See progress by account and stage whenever you check in.",
              ],
              [
                "02",
                "Keep a copy you can use.",
                "Export your full transaction history as CSV, PDF or JSON. Connect to Sure when you want it in your wider financial picture.",
              ],
              [
                "03",
                "Hear when it needs you.",
                "Optional Telegram or Pushover alerts tell you when a sync fails and when it recovers. No balances or credentials in notifications.",
              ],
              [
                "04",
                "Explore what comes next.",
                "Adjust contributions, time and an assumed return in the simulator. Keep hypothetical growth separate from your recorded portfolio.",
              ],
            ].map(([number, title, description]) => (
              <article key={number}>
                <span>{number}</span>
                <div>
                  <h3>{title}</h3>
                  <p>{description}</p>
                </div>
              </article>
            ))}
          </div>
        </section>
        <Architecture />
        <section id="setup" className="setup-section section-frame">
          <div className="section-caption">
            <span>[03] FROM TERMINAL TO DASHBOARD</span>
            <span>Docker · your computer or a VPS</span>
          </div>
          <div className="setup-grid">
            <div className="setup-copy">
              <h2>
                Your server.
                <br />
                Your setup.
              </h2>
              <p>
                Start with the <a href="#agent-setup">agent setup prompt</a> or the commands below,
                then sign in to DEGIRO in your own dashboard.
              </p>
              <a
                className="text-link"
                href={`${repository}/blob/main/docs/usage.md#docker-usage`}
                target="_blank"
                rel="noreferrer"
              >
                Setup guide on GitHub <ArrowUpRight size={17} />
              </a>
              <p className="setup-footnote">
                For a manual install, run the commands from your checkout.
              </p>
            </div>
            <div className="terminal">
              <div className="terminal-heading">
                <TerminalWindow size={17} />
                <span>Quick start</span>
                <span>shell</span>
              </div>
              <p className="terminal-comment"># Start Degitide</p>
              <Command>docker compose up --build -d</Command>
              <p className="terminal-comment">
                # Create your account and connect DEGIRO
              </p>
              <Command>docker compose exec app degitide setup</Command>
              <Command>docker compose exec app degitide connect</Command>
              <Command>docker compose exec app degitide sync</Command>
              <div className="terminal-status">
                <span />
                Then open localhost:8391 in your browser.
              </div>
            </div>
          </div>
          <div className="remote-row">
            <div>
              <span className="eyebrow">Already on a VPS?</span>
              <p>Open a private tunnel from your laptop.</p>
            </div>
            <Command>degitide open --ssh user@your-vps</Command>
            <a
              href={`${repository}/blob/main/docs/usage.md#open-a-vps-dashboard-through-ssh`}
              target="_blank"
              rel="noreferrer"
              aria-label="Read the SSH setup guide on GitHub"
            >
              <ArrowUpRight size={22} />
            </a>
          </div>
        </section>
        <section className="faq section-frame" id="questions">
          <div>
            <p className="section-index">[04] GOOD TO KNOW</p>
            <h2>A few details.</h2>
          </div>
          <div>
            {questions.map(([question, answer]) => (
              <details key={question}>
                <summary>
                  {question}
                  <span aria-hidden="true">+</span>
                </summary>
                <p>{answer}</p>
              </details>
            ))}
          </div>
        </section>
        <section className="closing section-frame">
          <div>
            <p className="section-index">YOURS TO RUN. YOURS TO KEEP.</p>
            <h2>
              Make yourself
              <br />
              at home.
            </h2>
          </div>
          <a
            className="button primary"
            href={repository}
            target="_blank"
            rel="noreferrer"
          >
            View on GitHub <ArrowUpRight size={18} />
          </a>
        </section>
      </main>
      <footer className="site-footer">
        <Brand />
        <div className="footer-credit">
          <p>Built by <a href="https://apogliaghi.com/?ref=degitide" rel="author">Alessandro Pogliaghi</a> <Robot size={16} aria-hidden="true" /></p>
          <p>Independent project. Unaffiliated with DEGIRO.</p>
        </div>
        <nav className="footer-links" aria-label="Footer">
          <a href="#setup">Setup</a>
          <a href="/llms.txt">For agents</a>
        </nav>
      </footer>
    </div>
  );
}
