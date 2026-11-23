// The diagram is illustrative only: it never contacts broker or app APIs.
export function createArchitectureFlow(root) {
  const byId = id => root.querySelector(`#arch-${id}`);
  const topology = byId("topology"), board = byId("board"), svg = byId("connections");
  const nodes = Object.fromEntries(["app", "worker", "proxy", "broker", "store", "sure"].map(id => [id, byId("node-" + id)]));
  const reduced = matchMedia("(prefers-reduced-motion: reduce)");
  const cleanups = [];
  const listen = (target, event, callback) => {
    target.addEventListener(event, callback);
    cleanups.push(() => target.removeEventListener(event, callback));
  };
 const scenarios={
  sync:[
   {edge:'app-worker',target:'worker',title:'Scheduler wakes the worker',copy:'Hourly while a held market is open, by default.'},
   {edge:'worker-proxy',target:'proxy',title:'A read request reaches the proxy',copy:'The worker requests holdings, transactions or portfolio reports.'},
   {edge:'proxy-broker',target:'broker',verdict:'allowed',title:'Reviewed request allowed',copy:'The method, path, query and bounded body match the policy.'},
   {edge:'proxy-broker',reverse:true,target:'proxy',response:true,title:'DEGIRO sends a response',copy:'Collection time does not make delayed broker quotes live.'},
   {edge:'worker-proxy',reverse:true,target:'worker',response:true,title:'Records return to the worker',copy:'Broker responses become validated holdings and transactions.'},
   {edge:'app-worker',reverse:true,target:'app',response:true,title:'The app receives a coherent snapshot',copy:'Holdings, transactions and broker totals are kept together.'},
   {edge:'app-store',target:'store',response:true,title:'Your local copy is updated',copy:'The dashboard reads SQLite. Sure can import this saved snapshot later.'}
  ],
  blocked:[
   {edge:'worker-proxy',target:'proxy',title:'An unreviewed request reaches the proxy',copy:'Example: an order request, outside the read/report allowlist.'},
   {target:'proxy',verdict:'blocked',title:'Denied here. Nothing sent to DEGIRO.',copy:'Known order routes and every unreviewed request are rejected.'}
  ],
  sure:[
   {edge:'app-sure',reverse:true,target:'app',title:'Sure requests its scoped feed',copy:'Sure runs its own import schedule; it contacts the app, not DEGIRO.'},
   {edge:'app-store',target:'store',title:'The app reads the saved snapshot',copy:'The browser worker and policy proxy stay idle.'},
   {edge:'app-store',reverse:true,target:'app',response:true,title:'Saved records return to the app',copy:'The feed applies account scope and freshness checks.'},
   {edge:'app-sure',target:'sure',response:true,title:'Sure receives your portfolio records',copy:'No DEGIRO credentials or worker access are shared.'}
  ]
 };
  let scenario = "sync", index = 0, playing = false, started = false, visible = false;
  let progress = 0, lastFrame = 0, animation = 0, disposed = false;
  const paths = {};
  const nodeRect = id => {
    const n = nodes[id].getBoundingClientRect(), r = topology.getBoundingClientRect();
    return { x: n.x-r.x, y: n.y-r.y, w: n.width, h: n.height, cx: n.x-r.x+n.width/2, cy: n.y-r.y+n.height/2 };
  };
  function connect(a, b) {
    const A = nodeRect(a), B = nodeRect(b);
    // Nodes in a horizontal row share the same CSS grid track and centre.
    // H/V segments also prevent subpixel slopes after responsive resizing.
    return Math.abs(A.cy-B.cy) > 40
      ? `M ${A.cx} ${A.y+A.h} V ${B.y}`
      : `M ${A.x+A.w} ${A.cy} H ${B.x}`;
  }
  function layout() {
    if (disposed) return;
    const r = topology.getBoundingClientRect();
    svg.setAttribute("viewBox", `0 0 ${r.width} ${r.height}`);
    const a = nodeRect("app"), s = nodeRect("store"), u = nodeRect("sure");
    const narrow = matchMedia("(max-width:760px)").matches;
    const spec = {
      "app-worker": connect("app", "worker"), "worker-proxy": connect("worker", "proxy"),
      "proxy-broker": connect("proxy", "broker"),
      "app-store": narrow ? `M ${a.x} ${a.cy} H ${a.x-9} V ${s.cy} H ${s.x}` : `M ${a.cx} ${a.y+a.h} V ${s.y}`,
      "app-sure": narrow ? `M ${a.x+a.w} ${a.cy} H ${u.x-16} V ${u.cy} H ${u.x}` : `M ${a.x+a.w*.78} ${a.y+a.h} V ${a.y+a.h+32} H ${u.cx} V ${u.y}`,
    };
    for (const [id, d] of Object.entries(spec)) {
      if (!paths[id]) {
        const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
        path.classList.add("arch-edge"); path.id = "arch-edge-" + id;
        byId("edge-layer").appendChild(path); paths[id] = path;
      }
      paths[id].setAttribute("d", d);
    }
    positionPacket();
  }
  function positionPacket() {
    const step = scenarios[scenario][index], path = paths[step.edge];
    const visible = !!path && started && !reduced.matches;
    for (const id of ["packet", "packet-glow"]) {
      const packet = byId(id); packet.toggleAttribute("hidden", !visible);
      if (path) {
        const t = Math.min(1, progress/.72), eased = t*t*(3-2*t);
        const pos = path.getPointAtLength(path.getTotalLength()*(step.reverse ? 1-eased : eased));
        packet.setAttribute("cx", pos.x); packet.setAttribute("cy", pos.y);
        packet.setAttribute("class", "arch-packet" + (step.response ? " response" : ""));
      }
    }
  }
  function draw() {
    const steps = scenarios[scenario], step = steps[index];
    board.dataset.scenario = scenario;
    Object.values(nodes).forEach(n => { n.dataset.active = "false"; delete n.dataset.result; });
    nodes[step.target].dataset.active = "true";
    if (step.verdict) nodes.proxy.dataset.result = step.verdict;
    byId("proxy-verdict").textContent = step.verdict === "blocked" ? "×" : step.verdict === "allowed" ? "✓" : "◇";
    Object.values(paths).forEach(path => path.setAttribute("class", "arch-edge"));
    if (paths[step.edge]) paths[step.edge].setAttribute("class", "arch-edge on" + (step.response ? " response" : ""));
    byId("step-count").textContent = String(index+1).padStart(2,"0") + "/" + String(steps.length).padStart(2,"0");
    byId("status-title").textContent = step.title; byId("status-copy").textContent = step.copy;
    byId("step-dots").replaceChildren(...steps.map((_, i) => { const dot = document.createElement("span"); dot.classList.toggle("current", i <= index); return dot; }));
    positionPacket();
  }
  function tick(now) {
    if (!playing || disposed) return;
    const delta = lastFrame ? Math.min(now-lastFrame, 80) : 0; lastFrame = now;
    progress += delta/(index === scenarios[scenario].length-1 ? 2400 : 1250);
    if (progress >= 1) {
      index = (index+1) % scenarios[scenario].length; progress = 0; draw();
    }
    positionPacket(); animation = requestAnimationFrame(tick);
  }
  function start() {
    cancelAnimationFrame(animation); lastFrame = 0; draw();
    if (playing && !reduced.matches) animation = requestAnimationFrame(tick);
  }
  function choose(value) {
    scenario = value; index = reduced.matches ? scenarios[scenario].length-1 : 0; progress = 0; started = true; playing = visible && !document.hidden && !reduced.matches;
    root.querySelectorAll("button[data-scenario]").forEach(b => b.setAttribute("aria-pressed", String(b.dataset.scenario === scenario)));
    start();
  }
  root.querySelectorAll("button[data-scenario]").forEach(button => listen(button, "click", () => choose(button.dataset.scenario)));
  root.querySelectorAll("button[data-mode]").forEach(button => listen(button, "click", () => {
    const split = button.dataset.mode === "split";
    board.classList.toggle("split", split);
    root.querySelectorAll("button[data-mode]").forEach(b => b.setAttribute("aria-pressed", String(b === button)));
    byId("host-label").textContent = split ? "Your host · separate containers" : "One container · shared network";
    byId("mode-badge").textContent = split ? "Advanced / enforced isolation" : "Default / simple install";
    byId("deployment-copy").textContent = split
      ? "Separate containers. Restricted paths and key mounts. Internal networks force worker traffic through the proxy; the worker has no direct internet route. The app and proxy do not mount the worker’s vault key."
      : "One container. One persistent volume. Simple to install, upgrade and back up. Browser traffic is configured through the proxy, but processes share a network and storage. This mode does not enforce the split deployment’s isolation.";
    layout();
  }));
  function updatePlayback() {
    playing = visible && !document.hidden && !reduced.matches;
    if (playing) started = true;
    if (reduced.matches) index = scenarios[scenario].length-1;
    start();
  }
  listen(reduced, "change", updatePlayback);
  listen(document, "visibilitychange", updatePlayback);
  const resize = new ResizeObserver(layout); resize.observe(topology);
  // Flow continuously while visible; don't spend animation frames offscreen.
  const visibility = new IntersectionObserver(entries => {
    visible = entries.some(entry => entry.isIntersecting);
    updatePlayback();
  }, { threshold: 0.15 });
  visibility.observe(board);
  document.fonts.ready.then(layout);
  layout(); draw();
  return () => {
    disposed = true; cancelAnimationFrame(animation); resize.disconnect(); visibility.disconnect();
    cleanups.forEach(cleanup => cleanup()); byId("edge-layer").replaceChildren();
  };
}
