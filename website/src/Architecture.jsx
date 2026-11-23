import { useEffect, useRef } from "react";
import { createArchitectureFlow } from "./architecture-flow.js";
import "./architecture.css";

const repository = "https://github.com/tatoalo/degitide";

export function Architecture() {
  const root = useRef(null);
  useEffect(() => createArchitectureFlow(root.current), []);

  return (
    <section id="architecture" className="architecture section-frame" ref={root} aria-labelledby="architecture-title">
      <div className="arch-intro">
        <div>
          <p className="section-index">[02] UNDER THE SURFACE</p>
          <h2 id="architecture-title">Follow the flow.<br />Keep the control.</h2>
        </div>
        <p>A worker collects your portfolio. A policy proxy checks its requests. Your records come home to a database you control. Watch what happens between those steps.</p>
      </div>
      <div className="arch-settings">
        <div className="arch-switch" role="group" aria-label="Deployment">
          <button type="button" data-mode="simple" aria-pressed="true">Simple install</button>
          <button type="button" data-mode="split" aria-pressed="false">Isolated deployment</button>
        </div>
        <span className="arch-label">Same data flow. Different boundaries.</span>
      </div>
      <div className="arch-board" id="arch-board" data-scenario="sync">
        <div className="arch-topology" id="arch-topology">
          <svg className="arch-connections" id="arch-connections" aria-hidden="true">
            <g id="arch-edge-layer" />
            <circle id="arch-packet-glow" r="11" opacity=".12" className="arch-packet" hidden />
            <circle id="arch-packet" r="4" className="arch-packet" hidden />
          </svg>
          <div className="arch-boundary" aria-hidden="true"><span id="arch-host-label">One container · shared network</span></div>
          <div className="arch-node" id="arch-node-app" data-container="app">
            <div className="arch-node-label"><span>01 / Coordinate</span><b aria-hidden="true">→</b></div>
            <h3>App &amp; scheduler</h3>
            <span className="arch-node-detail">Dashboard · CLI<br />Market-aware sync</span>
            <span className="arch-container-tag">App container</span>
          </div>
          <div className="arch-node" id="arch-node-worker" data-container="worker">
            <div className="arch-node-label"><span>02 / Collect</span><b aria-hidden="true">↻</b></div>
            <h3>Browser worker</h3>
            <span className="arch-node-detail">Session · holdings<br />Trades · reports</span>
            <span className="arch-container-tag">Worker container</span>
          </div>
          <div className="arch-node" id="arch-node-proxy" data-container="proxy">
            <div className="arch-node-label"><span>03 / Inspect</span><b id="arch-proxy-verdict" aria-hidden="true">◇</b></div>
            <h3>Policy proxy</h3>
            <span className="arch-node-detail">Method · path<br />Query · bounded body</span>
            <span className="arch-container-tag">Proxy container</span>
          </div>
          <div className="arch-node" id="arch-node-broker">
            <div className="arch-node-label"><span>External source</span><b aria-hidden="true">↗</b></div>
            <h3><img className="arch-broker-logo" src="/assets/degiro.svg" alt="DEGIRO" width="144" height="20" /></h3>
            <span className="arch-node-detail">Broker endpoints<br />Approved quote services</span>
          </div>
          <div className="arch-node arch-small" id="arch-node-store">
            <div className="arch-storage-mark">Persistent copy</div>
            <h3>SQLite</h3><span className="arch-node-detail">Your saved records</span>
          </div>
          <div className="arch-policy-foot">
            <span className="arch-pass">+ Reviewed account reads, reports &amp; quotes</span><br />
            <span className="arch-deny">× Orders &amp; unreviewed requests</span><br />
            <span>Sign-in has a separate, temporary permission.</span>
          </div>
          <div className="arch-node arch-small" id="arch-node-sure">
            <div className="arch-storage-mark">Optional consumer</div>
            <h3>Sure</h3><span className="arch-node-detail">Scoped SimpleFIN feed</span>
          </div>
        </div>
        <div className="arch-status">
          <span id="arch-step-count" className="arch-step-count">01/07</span>
          <div><p id="arch-status-title">Scheduler wakes the worker</p><p className="arch-status-copy" id="arch-status-copy">Hourly while a held market is open, by default.</p></div>
          <div className="arch-dots" id="arch-step-dots" aria-hidden="true" />
        </div>
      </div>
      <div className="arch-settings">
        <div className="arch-switch" role="group" aria-label="Flow scenario">
          <button type="button" data-scenario="sync" aria-pressed="true">Portfolio sync</button>
          <button type="button" data-scenario="blocked" aria-pressed="false">Blocked request</button>
          <button type="button" data-scenario="sure" aria-pressed="false">Sure import</button>
        </div>
        <span className="arch-label">Select a flow to replay it</span>
      </div>
      <div className="arch-deployment">
        <p id="arch-deployment-copy">One container. One persistent volume. Simple to install, upgrade and back up. Browser traffic is configured through the proxy, but processes share a network and storage. This mode does not enforce the split deployment’s isolation.</p>
        <span id="arch-mode-badge">Default / simple install</span>
      </div>
      <div className="arch-facts">
        <article><h3>Nothing new is allowed by default.</h3><p>Unreviewed broker requests stop at the proxy. Supporting a new route needs a deliberate policy change.</p></article>
        <article><h3>Your records stay with you.</h3><p>SQLite stores portfolio records. Saved credentials and session checkpoints are encrypted at rest; protect the host, keys and backups.</p></article>
        <article><h3>Sure reads your saved snapshot.</h3><p>Its import is separate from broker sync. Sure receives scoped records from the app, without your DEGIRO login or access to the worker.</p></article>
      </div>
      <div className="arch-foot"><a href={`${repository}/blob/main/docs/split-deployment.md`} target="_blank" rel="noreferrer">Deployment guide ↗</a></div>
    </section>
  );
}
