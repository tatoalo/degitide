"""Exercise portfolio observation and full replay in real Chromium, offline.

Run: .venv/bin/python tests/browser/valuation_sync.py
All browser and API requests are served by the local synthetic broker below.
No broker credentials, live sessions, or external network requests are used.
"""
from __future__ import annotations

import asyncio
from collections import Counter
import json
from pathlib import Path
import sys
from unittest.mock import patch
from urllib.parse import urlsplit

from aiohttp import web
from patchright.async_api import async_playwright

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "worker"))
from proxy import policy as egress_policy  # noqa: E402
from tasks.common import TaskError  # noqa: E402
from tasks.holdings import fetch  # noqa: E402
from tasks.valuation import apply_frontend  # noqa: E402


ACCOUNT_PATH = "/trading/secure/v5/account/info/123;jsessionid=fixture"
UPDATE_PATH = "/trading/secure/v5/update/123;jsessionid=fixture"
UPDATE_QUERY = "?portfolio=81&totalPortfolio=67&intAccount=123&sessionId=fixture"
WORKER_PATH = "/trader/scripts/event-broker-worker.0123456789abcdef0123.js"
PRODUCT_PATH = "/productsearch/secure/v1/products/info"
SECRETS = ["synthetic-session-secret", "synthetic-password-secret", "synthetic-cookie-secret"]


def raw_portfolio():
    row = {"id": "100", "positionType": "PRODUCT", "size": 2, "price": 110,
           "value": 200, "breakEvenPrice": 90}
    return {"portfolio": {"isAdded": True, "value": [{"id": "100", "name": "positionrow", "isAdded": True,
        "value": [{"name": key, "value": value, "isAdded": True} for key, value in row.items()]}]}}


def calculated_position():
    return {"id": "100", "positionType": "PRODUCT", "size": 2, "price": 115,
            "value": 210, "valueInProductCurrency": 230, "breakEvenPrice": 90,
            "lastUpdate": "2026-11-16T16:05:00Z", "isActive": True,
            "sessionId": SECRETS[0], "cookie": SECRETS[2],
            "productInfo": {"id": "100", "symbol": "TEST", "currency": "USD",
                            "contractSize": 1, "productTypeId": 131, "closePrice": 110,
                            "closePriceDate": "2026-11-13", "password": SECRETS[1]}}


class SyntheticBroker:
    def __init__(self, kind="Worker", scenario="quoted"):
        self.kind, self.scenario = kind, scenario
        self.requests = []
        self.product_lookups = []
        self.origin = ""

    def url(self, remote):
        parts = urlsplit(remote)
        assert parts.scheme == "https" and parts.hostname == "trader.degiro.nl", remote
        return self.origin + parts.path + ("?" + parts.query if parts.query else "")

    async def request(self, request):
        transport = request.headers.get("X-Synthetic-Transport", "unknown")
        self.requests.append((transport, request.method, request.path, dict(request.query)))
        if request.path == "/trader/":
            return web.Response(text=self.html(), content_type="text/html")
        if request.path == ACCOUNT_PATH:
            return web.json_response({"data": {"baseCurrency": "EUR"}})
        if request.path == "/login/secure/config":
            return web.json_response({"data": {"dictionaryUrl": "https://trader.degiro.nl/product_search/config/dictionary/"}})
        if request.path == "/product_search/config/dictionary/":
            decision = egress_policy.decide(request.method, "https://trader.degiro.nl" + request.path_qs)
            assert decision.allowed, decision
            return web.json_response({"exchanges": [{"id": 194, "micCode": "XETR", "name": "Xetra"}]})
        if request.path == UPDATE_PATH:
            if transport == "api" and self.scenario == "failed-replay":
                return web.json_response({"error": "Synthetic unavailable response"}, status=503)
            return web.json_response(raw_portfolio())
        if request.path == PRODUCT_PATH:
            assert request.method == "POST"
            if self.scenario in ("mixed-products", "blocked-mixed-products"):
                body = await request.read()
                ids = json.loads(body)
                assert ids == ["100", "EUR", "FLATEX_EUR"], ids
                url = "https://trader.degiro.nl" + request.path_qs
                if self.scenario == "blocked-mixed-products":
                    # Reproduce the previous numeric-only body validator while
                    # retaining the actual route, query and decision logic.
                    with patch.object(egress_policy, "_valid_product_body", lambda body: all(
                        isinstance(item, str) and item.isdigit() for item in json.loads(body)
                    )):
                        decision = egress_policy.decide("POST", url, body=body)
                else:
                    decision = egress_policy.decide("POST", url, body=body)
                self.product_lookups.append((ids, decision))
                if not decision.allowed:
                    return web.json_response({"error": decision.reason}, status=403)
                return web.json_response({"data": {
                    "100": calculated_position()["productInfo"],
                    "EUR": {"id": "EUR", "currency": "EUR"},
                    "FLATEX_EUR": {"id": "FLATEX_EUR", "currency": "EUR"},
                }})
            assert await request.json() == ["100"]
            return web.json_response({"data": {"100": {
                "id": "100", "symbol": "TEST", "currency": "USD",
                "name": "Synthetic investment", "isin": "IE0000000000",
                "exchangeId": 194, "productTypeId": 131,
            }}})
        if request.path == WORKER_PATH:
            return web.Response(text=self.worker(), content_type="application/javascript")
        raise web.HTTPNotFound()

    def html(self):
        if self.scenario == "lifecycle":
            return "<!doctype html><title>Synthetic worker lifecycle</title>"
        config = {"currentClient": {"id": 9001, "intAccount": 123, "accountInfo": {"baseCurrency": "EUR"}},
                  "appConfig": {"sessionId": SECRETS[0]}, "password": SECRETS[1]}
        return f"""<!doctype html><title>Synthetic portfolio</title><script>
window.synthetic = {{sent: 0, received: 0, acknowledged: 0}};
(async () => {{
  await fetch({json.dumps(ACCOUNT_PATH)});
  await fetch({json.dumps(UPDATE_PATH + UPDATE_QUERY)});
  const worker = new {self.kind}({json.dumps(WORKER_PATH)});
  window.syntheticWorker = worker;
  const port = worker.port || worker;
  port.addEventListener('message', (message) => {{
    window.synthetic.received++;
    const event = JSON.parse(message.data);
    if (event.data?.event === 'synthetic:ack') window.synthetic.acknowledged = event.data.data.received;
    if (event.data?.event === 'synthetic:readAudit') window.synthetic.readAudit = event.data.data;
    if (event.data?.params?.degitideSnapshot) window.synthetic.snapshotReplies = (window.synthetic.snapshotReplies || 0) + 1;
  }});
  port.start?.();
  port.postMessage(JSON.stringify({{action: 'SUBSCRIBE_MESSAGE_PORT', data: {{clientId:'9001', eventBrokerConfig: {json.dumps(config)}}}}}));
  window.synthetic.sent++;
}})();
</script>"""

    def worker(self):
        if self.scenario == "lifecycle":
            return """
function wire(port) {
  let account = 0, received = 0;
  const send = (event, data) => port.postMessage(JSON.stringify({action: 'EMIT_EVENT', data: {event, data}}));
  port.onmessage = ({data}) => {
    received++;
    const message = JSON.parse(data);
    const config = message.data?.eventBrokerConfig;
    if (config) account = config.currentClient.intAccount;
    if (message.action === 'fixture:expire')
      port.postMessage(JSON.stringify({action: 'NOTIFY_ON_MAX_FAILED_UPDATE_REQUESTS'}));
    else {
      send('portfolio:reset', {activePositions: {'100': {id: '100', positionType: 'PRODUCT', size: 2, value: account}}});
      send('totalPortfolio:reset', {portfolio: account, total: account, totalCash: 0, totalPl: account, todayPl: 0});
    }
    send('synthetic:ack', {received});
  };
  port.start?.();
}
if (typeof SharedWorkerGlobalScope !== 'undefined' && self instanceof SharedWorkerGlobalScope)
  self.onconnect = (event) => wire(event.ports[0]);
else wire(self);
"""
        position = calculated_position()
        totals = {"portfolio": 210, "total": 222.50, "totalCash": 12.50,
                  "totalPl": 32.17, "todayPl": -1.23, "availableToSpend": 50.25,
                  "password": SECRETS[1]}
        lookup_gate = ""
        if self.scenario in ("mixed-products", "blocked-mixed-products"):
            # Native worker 5.144.0 resolves every added position, including
            # cash pseudo-products, before emitting enriched portfolio events.
            lookup_gate = f"""
    send('portfolio:lastData', {{activePositions: {{'100': {{id:'100', positionType:'PRODUCT', size:2, price:0, value:200}}}}, inactivePositions: {{}}}});
    send('totalPortfolio:lastData', {{totalCash:12.50}});
    const response = await fetch({json.dumps(PRODUCT_PATH + '?intAccount=123&sessionId=fixture')}, {{
      method:'POST', headers:{{'Content-Type':'application/json'}},
      body:JSON.stringify(['100','EUR','FLATEX_EUR']),
    }});
    if (!response.ok) {{ send('synthetic:ack', {{received}}); return; }}
    const products = (await response.json()).data;
    if (!products['100']) throw new Error('Missing product metadata');
"""
        return f"""
function wire(port) {{
  let received = 0, subscriptions = 0, unsubscriptions = 0, portfolioReads = 0;
  const markers = [], active = new Set();
  const portfolio = {{activePositions: {{'100': {json.dumps(position)}}}, inactivePositions: {{}}}};
  const totals = {json.dumps(totals)};
  const send = (event, data, params) => port.postMessage(JSON.stringify({{action: 'EMIT_EVENT', data: {{event, data, params}}}}));
  port.onmessage = async ({{data}}) => {{
    received++;
    const message = JSON.parse(data);
    if (message.action === 'SUBSCRIBE_RESOURCE' || message.action === 'UNSUBSCRIBE_RESOURCE') {{
      const {{clientId,event,params}} = message.data;
      if (clientId !== '9001') return;
      const key = event + JSON.stringify(params);
      if (message.action === 'SUBSCRIBE_RESOURCE') {{
        active.add(key); subscriptions++;
        if (!markers.includes(params.degitideSnapshot)) markers.push(params.degitideSnapshot);
        let value = event === 'portfolio:lastData' ? portfolio : totals;
        if (event === 'portfolio:lastData') {{
          portfolioReads++;
          if ({json.dumps(self.scenario)} === 'interleaved' && portfolioReads === 1) {{
            value = structuredClone(portfolio);
            value.activePositions['100'].value = 200;
          }}
        }}
        send(event, value, params);
      }} else {{
        active.delete(key); unsubscriptions++;
        if (unsubscriptions % 2 === 0) send('synthetic:readAudit', {{
          received, subscriptions, unsubscriptions, active:active.size, markers,
        }});
      }}
      return;
    }}
    if (message.action !== 'SUBSCRIBE_MESSAGE_PORT') return;
    {lookup_gate}
    if ({json.dumps(self.scenario)} === 'fresh-snapshot') {{
      const initial = structuredClone(portfolio);
      Object.assign(initial.activePositions['100'], {{value:200, price:110, lastUpdate:'2026-11-13T00:00:00Z'}});
      send('portfolio:lastData', initial);
      send('totalPortfolio:lastData', totals);
    }} else {{
      send('portfolio:reset', portfolio);
      send('totalPortfolio:reset', totals);
    }}
    send('other:private', {{password: {json.dumps(SECRETS[1])}}});
    send('synthetic:ack', {{received}});
  }};
  port.start?.();
}}
if (typeof SharedWorkerGlobalScope !== 'undefined' && self instanceof SharedWorkerGlobalScope)
  self.onconnect = (event) => wire(event.ports[0]);
else wire(self);
"""


class SnapshotBroker(SyntheticBroker):
    """Model native lastData reads separately from unsolicited UI events."""

    def html(self):
        return "<!doctype html><title>Synthetic snapshot races</title>"

    def worker(self):
        return """
const fullPosition = POSITION;
function wire(port) {
  let clientId = '', mode = 'fresh';
  const active = new Set(), requests = [];
  const send = (event, data, params) => port.postMessage(JSON.stringify({action:'EMIT_EVENT', data:{event,data,params}}));
  const value = (event, amount = 210) => event === 'portfolio:lastData'
    ? {activePositions:{'100':{...fullPosition,value:amount}},inactivePositions:{}}
    : {portfolio:amount,total:amount+12.50,totalCash:12.50,totalPl:32.17,todayPl:-1.23};
  port.onmessage = ({data}) => {
    const message = JSON.parse(data);
    const reply = () => send('fixture:result', {requestId:message.requestId, active:active.size, requests});
    if (message.action === 'SUBSCRIBE_MESSAGE_PORT') {
      clientId = message.data.clientId;
      const stale = value('portfolio:lastData', 200);
      Object.assign(stale.activePositions['100'], {price:110,lastUpdate:'2026-11-13T00:00:00Z'});
      send('portfolio:lastData', stale);
      send('totalPortfolio:lastData', value('totalPortfolio:lastData'));
      reply(); return;
    }
    if (message.action === 'fixture:mode') { mode = message.mode; reply(); return; }
    if (message.action === 'fixture:stats') { reply(); return; }
    if (!['SUBSCRIBE_RESOURCE','UNSUBSCRIBE_RESOURCE'].includes(message.action)) return;
    const {event,params} = message.data;
    requests.push({action:message.action, ...message.data});
    if (message.data.clientId !== clientId) return;
    const key = event + JSON.stringify(params);
    if (message.action === 'UNSUBSCRIBE_RESOURCE') { active.delete(key); return; }
    active.add(key);
    if (mode === 'silent') return;
    if (mode === 'expire') {
      if (event === 'portfolio:lastData') port.postMessage(JSON.stringify({action:'NOTIFY_ON_EXPIRED_SESSION'}));
      return;
    }
    if (mode === 'late') { setTimeout(() => send(event,value(event,999),params),1600); return; }
    if (mode === 'noise') {
      send(event,value(event,888));
      send(event,value(event,777),{degitideSnapshot:'unrelated-marker'});
      send(event,value(event,666),{...params,productTypeId:999});
      send(event.replace(':lastData',':change'),value(event,555),params);
      setTimeout(() => send(event,value(event),params),300); return;
    }
    send(event,value(event,mode === 'mismatch' && event === 'totalPortfolio:lastData' ? 211 : 210),params);
  };
  port.start?.();
}
if (typeof SharedWorkerGlobalScope !== 'undefined' && self instanceof SharedWorkerGlobalScope)
  self.onconnect = event => wire(event.ports[0]);
else wire(self);
""".replace("POSITION", json.dumps(calculated_position()))


class RequestBridge:
    def __init__(self, context, broker):
        self.request, self.broker = context.request, broker

    async def get(self, url, **kwargs):
        kwargs["headers"] = {**kwargs.get("headers", {}), "X-Synthetic-Transport": "api"}
        return await self.request.get(self.broker.url(url), **kwargs)

    async def post(self, url, **kwargs):
        kwargs["headers"] = {**kwargs.get("headers", {}), "X-Synthetic-Transport": "api"}
        return await self.request.post(self.broker.url(url), **kwargs)


class ObservedPage:
    def __init__(self, page, owner):
        self.page, self.owner = page, owner

    def __getattr__(self, name):
        return getattr(self.page, name)

    async def close(self):
        self.owner.observed = await self.page.evaluate(
            """async () => {
              const deadline = Date.now() + 1000;
              while (window.synthetic?.snapshotReplies &&
                     window.synthetic?.readAudit?.unsubscriptions !== window.synthetic.snapshotReplies) {
                if (Date.now() > deadline) throw new Error('Snapshot subscriptions were not cleaned up');
                await new Promise(resolve => setTimeout(resolve, 10));
              }
              return {portfolio:window.__degitidePortfolio, traffic:window.synthetic};
            }""", isolated_context=False,
        )
        await self.page.close()


class ContextBridge:
    def __init__(self, context, broker):
        self.context = context
        self.request = RequestBridge(context, broker)
        self.observed = None

    async def new_page(self):
        return ObservedPage(await self.context.new_page(), self)


async def exercise(browser, kind, scenario):
    broker = SyntheticBroker(kind, scenario)
    app = web.Application()
    app.router.add_route("*", "/{path:.*}", broker.request)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    broker.origin = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
    context = await browser.new_context()
    unexpected = []

    async def route(request):
        try:
            local_url = broker.url(request.request.url)
        except AssertionError:
            unexpected.append(request.request.url)
            await request.abort()
            return
        response = await request.fetch(url=local_url, headers={
            **request.request.headers, "X-Synthetic-Transport": "browser",
        })
        await request.fulfill(response=response)

    await context.route("**/*", route)
    bridge = ContextBridge(context, broker)
    try:
        if scenario == "failed-replay":
            try:
                await fetch(bridge)
                raise AssertionError("Failed replay was accepted")
            except TaskError as error:
                assert "503" in str(error), error
        else:
            result = await fetch(bridge)
            assert result["holdings"][0]["market_value"] == 210, (result, bridge.observed, broker.requests)
            assert result["holdings"][0]["currency"] == "USD", result
            assert result["holdings"][0]["value_currency"] == "EUR", result
            assert result["holdings"][0]["price"] == 115, result
            assert result["valuation"]["account_value"] == 222.50, result
            assert result["valuation"]["total_pl"] == 32.17, result
            assert result["valuation"]["price_status"] == "quoted", result
            assert result["holdings"][0]["exchange_mic"] == "XETR", result
            assert result["market_schedule"]["unknown_holdings"] == 0, result
            assert result["market_schedule"]["markets"][0]["mic"] == "XETR", result
        state = bridge.observed
        attempts = 0 if scenario == "failed-replay" else 2 if scenario == "interleaved" else 1
        assert state["traffic"]["sent"] == 1 and state["traffic"]["acknowledged"] == 1, state
        assert state["traffic"]["received"] == 4 + attempts * 3, state
        if attempts:
            audit = state["traffic"]["readAudit"]
            assert audit["received"] == 1 + attempts * 4, audit
            assert audit["subscriptions"] == audit["unsubscriptions"] == attempts * 2, audit
            assert audit["active"] == 0 and len(set(audit["markers"])) == attempts, audit
        for secret in SECRETS:
            assert secret not in json.dumps(state["portfolio"]), state
        assert set(state["portfolio"]) <= {
            "positions", "totals", "base_currency", "int_account", "changed_at", "totals_at", "error",
        }, state
        assert not context.pages, "Fetch leaked its browser page"
        assert not unexpected, unexpected
        replays = [request for request in broker.requests if request[0] == "api" and request[2] == UPDATE_PATH]
        assert len(replays) == 1 and replays[0][3]["portfolio"] == "0" and replays[0][3]["totalPortfolio"] == "0", replays
        traffic = Counter((transport, method, path) for transport, method, path, _ in broker.requests)
        expected = Counter({("browser", "GET", path): 1 for path in ("/trader/", ACCOUNT_PATH, UPDATE_PATH, WORKER_PATH)})
        expected[("api", "GET", UPDATE_PATH)] = 1
        if scenario != "failed-replay":
            expected[("api", "POST", PRODUCT_PATH)] = 1
            expected[("api", "GET", "/login/secure/config")] = 1
            expected[("api", "GET", "/product_search/config/dictionary/")] = 1
        assert traffic == expected, (traffic, expected)
        print(f"PASS {kind} {scenario}: actual observer, full replay, broker values, bounded snapshot reads and secret filtering")
    finally:
        await context.close()
        await runner.cleanup()


async def exercise_lifecycle(browser, kind):
    broker = SyntheticBroker(kind, "lifecycle")
    app = web.Application()
    app.router.add_route("*", "/{path:.*}", broker.request)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    broker.origin = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
    context = await browser.new_context()

    async def route(request):
        response = await request.fetch(url=broker.url(request.request.url))
        await request.fulfill(response=response)

    await context.route("**/*", route)
    try:
        page = await context.new_page()
        await page.add_init_script(path=str(ROOT / "worker/tasks/portfolio_observer.js"))
        await page.goto("https://trader.degiro.nl/trader/")
        result = await page.evaluate("""async ({kind, path}) => {
          const read = () => JSON.parse(JSON.stringify(window.__degitidePortfolio));
          const snapshots = {};
          let sent = 0;
          const post = (target, message) => new Promise((resolve) => {
            const onMessage = ({data}) => {
              const event = JSON.parse(data);
              if (event.data?.event === 'synthetic:ack') {
                target.removeEventListener('message', onMessage);
                resolve(event.data.data.received);
              }
            };
            target.addEventListener('message', onMessage);
            target.postMessage(JSON.stringify(message));
            sent++;
          });
          const start = async (account, name) => {
            const worker = new window[kind](path, {name});
            const target = worker.port || worker;
            target.start?.();
            const pending = post(target, {action: 'SUBSCRIBE_MESSAGE_PORT', data: {
              clientId:String(account + 10000),
              eventBrokerConfig: {currentClient: {id:account + 10000, intAccount: account, accountInfo: {baseCurrency: 'EUR'}}},
            }});
            snapshots[name + 'BeforeResponse'] = read();
            const received = await pending;
            return {worker, target, received};
          };
          const close = ({worker, target}) => worker.port ? target.close() : worker.terminate();
          const first = await start(123, 'first');
          snapshots.first = read();
          const second = await start(456, 'second');
          snapshots.second = read();
          const oldReceived = await post(first.target, {action: 'fixture:refresh'});
          snapshots.afterOldMessage = read();
          close(first);
          snapshots.afterOldClose = read();
          close(second);
          snapshots.afterCurrentClose = read();
          const third = await start(456, 'third');
          const expiredReceived = await post(third.target, {action: 'fixture:expire'});
          snapshots.afterStreamFailure = read();
          close(third);
          const fourth = await start(456, 'fourth');
          const unsubscribedReceived = await post(fourth.target, {action: 'UNSUBSCRIBE_MESSAGE_PORT'});
          snapshots.afterUnsubscribe = read();
          close(fourth);
          return {snapshots, sent, received: [first.received, second.received, oldReceived,
                  third.received, expiredReceived, fourth.received, unsubscribedReceived]};
        }""", {"kind": kind, "path": WORKER_PATH}, isolated_context=False)
        states = result["snapshots"]
        assert states["first"]["totals"]["totalPl"] == 123, result
        for name in ("secondBeforeResponse", "thirdBeforeResponse", "fourthBeforeResponse"):
            assert states[name]["int_account"] == "456", result
            assert states[name]["totals"] is None and states[name]["positions"] == {}, result
        for name in ("second", "afterOldMessage", "afterOldClose"):
            assert states[name]["int_account"] == "456" and states[name]["totals"]["totalPl"] == 456, result
        for name in ("afterCurrentClose", "afterStreamFailure", "afterUnsubscribe"):
            assert states[name]["totals"] is None and states[name]["positions"] == {}, result
            assert states[name]["error"], result
        assert result["sent"] == 7 and result["received"] == [1, 1, 2, 1, 2, 1, 2], result
        assert all(path in ("/trader/", WORKER_PATH) for _, _, path, _ in broker.requests), broker.requests
        print(f"PASS {kind} lifecycle: account/restart reset, ignored old events, close/failure/unsubscribe invalidation, passive traffic")
    finally:
        await context.close()
        await runner.cleanup()


async def exercise_product_lookup(browser, kind):
    """A cash ID in native metadata requests must not prevent all enrichment."""
    for scenario in ("blocked-mixed-products", "mixed-products"):
        broker = SyntheticBroker(kind, scenario)
        app = web.Application()
        app.router.add_route("*", "/{path:.*}", broker.request)
        runner = web.AppRunner(app, access_log=None)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        broker.origin = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
        context = await browser.new_context()
        try:
            page = await context.new_page()
            await page.add_init_script(path=str(ROOT / "worker/tasks/portfolio_observer.js"))
            # SharedWorker fetches are not exposed to page routing. A local
            # origin lets the real HTTP server enforce the policy for both.
            await page.goto(broker.origin + "/trader/")
            deadline = asyncio.get_running_loop().time() + 5
            while True:
                state = await page.evaluate(
                    "({portfolio: window.__degitidePortfolio, traffic: window.synthetic})",
                    isolated_context=False,
                )
                if state["traffic"]["acknowledged"] == 1:
                    break
                assert asyncio.get_running_loop().time() < deadline, (kind, scenario, state, broker.requests)
                await page.wait_for_timeout(20)
            original = [{"product_id": "100", "quantity": 2, "currency": "USD", "market_value": 200}]
            adopted = apply_frontend(original, state["portfolio"], "EUR")
            assert len(broker.product_lookups) == 1, broker.product_lookups
            ids, decision = broker.product_lookups[0]
            assert ids == ["100", "EUR", "FLATEX_EUR"] and decision.route == "product-info", broker.product_lookups
            if scenario == "blocked-mixed-products":
                assert not decision.allowed and decision.reason == "invalid product-info body", decision
                assert adopted is None, adopted
                assert state["portfolio"]["positions"]["100"]["price"] == 0, state
                assert "productInfo" not in state["portfolio"]["positions"]["100"], state
                assert state["portfolio"]["totals"] == {"totalCash": 12.50}, state
            else:
                assert decision.allowed and adopted is not None, (decision, state)
                assert adopted["holdings"][0]["market_value"] == 210, adopted
                assert adopted["valuation"]["account_value"] == 222.50, adopted
            assert state["traffic"]["sent"] == 1, state
        finally:
            await context.close()
            await runner.cleanup()
    print(f"PASS {kind} mixed product lookup: legacy policy blocks enrichment; current policy permits securities and cash metadata")


async def exercise_snapshot_races(browser, kind):
    broker = SnapshotBroker(kind)
    app = web.Application()
    app.router.add_route("*", "/{path:.*}", broker.request)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    broker.origin = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
    context = await browser.new_context()
    try:
        page = await context.new_page()
        await page.add_init_script(path=str(ROOT / "worker/tasks/portfolio_observer.js"))
        await page.goto(broker.origin + "/trader/")
        result = await page.evaluate("""async ({kind,path}) => {
          const results = {}, workers = [];
          let sequence = 0;
          const ask = (target, action, extra = {}) => new Promise((resolve,reject) => {
            const requestId = ++sequence;
            const timer = setTimeout(() => reject(new Error('Fixture response timeout')),4000);
            const receive = ({data}) => {
              const message = JSON.parse(data);
              if (message.data?.event === 'fixture:result' && message.data.data.requestId === requestId) {
                clearTimeout(timer); target.removeEventListener('message',receive); resolve(message.data.data);
              }
            };
            target.addEventListener('message',receive);
            target.postMessage(JSON.stringify({action,requestId,...extra}));
          });
          const start = async (account,clientId) => {
            const worker = new window[kind](path,{name:'snapshot-'+sequence+'-'+clientId});
            const target = worker.port || worker;
            workers.push({worker,target}); target.start?.();
            await ask(target,'SUBSCRIBE_MESSAGE_PORT',{data:{clientId,eventBrokerConfig:{
              currentClient:{id:Number(clientId),intAccount:account,accountInfo:{baseCurrency:'EUR'}},
            }}});
            return {worker,target};
          };
          const close = ({worker,target}) => worker.port ? target.close() : worker.terminate();
          const first = await start(123,'9001');
          results.passive = JSON.parse(JSON.stringify(window.__degitidePortfolio));
          const a = window.__degitideReadPortfolio(), b = window.__degitideReadPortfolio();
          results.sharedPromise = a === b;
          results.fresh = await a; await b;
          await ask(first.target,'fixture:mode',{mode:'mismatch'});
          results.mismatch = await window.__degitideReadPortfolio();
          await ask(first.target,'fixture:mode',{mode:'fresh'});
          results.retry = await window.__degitideReadPortfolio();
          await ask(first.target,'fixture:mode',{mode:'silent'});
          results.timeout = await window.__degitideReadPortfolio();
          results.afterTimeout = await ask(first.target,'fixture:stats');
          await ask(first.target,'fixture:mode',{mode:'late'});
          results.lateTimeout = await window.__degitideReadPortfolio();
          await ask(first.target,'fixture:mode',{mode:'noise'});
          results.afterNoiseAndLate = await window.__degitideReadPortfolio();
          results.firstAudit = await ask(first.target,'fixture:stats');
          await ask(first.target,'fixture:mode',{mode:'late'});
          const replaced = window.__degitideReadPortfolio();
          const second = await start(456,'9002');
          results.replaced = await replaced;
          results.newAccount = await window.__degitideReadPortfolio();
          results.firstAfterReplacement = await ask(first.target,'fixture:stats');
          await ask(second.target,'fixture:mode',{mode:'expire'});
          results.expired = await window.__degitideReadPortfolio();
          results.afterFailure = await window.__degitideReadPortfolio();
          results.secondAudit = await ask(second.target,'fixture:stats');
          const third = await start(456,'9003');
          await ask(third.target,'fixture:mode',{mode:'silent'});
          const stopped = window.__degitideReadPortfolio();
          close(third);
          results.stopped = await stopped;
          close(first); close(second);
          return results;
        }""", {"kind": kind, "path": WORKER_PATH}, isolated_context=False)
        original = [{"product_id": "100", "quantity": 2, "currency": "USD", "market_value": 200}]
        assert result["passive"]["positions"]["100"]["value"] == 200, result
        assert result["passive"]["totals"]["portfolio"] == 210, result
        assert result["sharedPromise"], "Concurrent reads duplicated the subscription pair"
        for name in ("fresh", "retry", "afterNoiseAndLate", "newAccount"):
            adopted = apply_frontend(original, result[name], "EUR")
            assert adopted is not None and adopted["holdings"][0]["market_value"] == 210, (name,result[name])
            assert adopted["valuation"]["account_value"] == 222.50, adopted
            assert adopted["holdings"][0]["price_as_of"] == "2026-11-16", adopted
        diagnostics = {}
        assert apply_frontend(original, result["mismatch"], "EUR", diagnostics=diagnostics) is None, result
        assert diagnostics["reason"] == "portfolio_value_mismatch", diagnostics
        for name in ("timeout", "lateTimeout"):
            assert result[name].get("snapshot_error") == "timeout", (name,result[name])
            assert apply_frontend(original, result[name], "EUR") is None, (name,result[name])
        for name in ("replaced", "expired", "stopped"):
            assert result[name].get("snapshot_error") == "stream_changed", (name,result[name])
            assert apply_frontend(original, result[name], "EUR") is None, (name,result[name])
        assert result["newAccount"]["int_account"] == "456", result
        assert result["replaced"]["int_account"] == "123", result
        assert result["afterFailure"]["snapshot_error"] == "worker_unavailable", result
        for name, pairs, client in (("afterTimeout",4,"9001"),("firstAudit",6,"9001"),
                                    ("firstAfterReplacement",7,"9001"),("secondAudit",2,"9002")):
            audit = result[name]
            assert audit["active"] == 0 and len(audit["requests"]) == pairs * 4, (name,audit)
            for start in range(0,len(audit["requests"]),4):
                group = audit["requests"][start:start+4]
                assert [row["action"] for row in group] == ["SUBSCRIBE_RESOURCE"]*2 + ["UNSUBSCRIBE_RESOURCE"]*2, group
                assert [row["event"] for row in group] == ["portfolio:lastData","totalPortfolio:lastData"]*2, group
                assert len({json.dumps(row["params"],sort_keys=True) for row in group}) == 1, group
                assert all(row["clientId"] == client for row in group), group
            assert len({row["params"]["degitideSnapshot"] for row in audit["requests"]}) == pairs, audit
        for secret in SECRETS:
            assert secret not in json.dumps(result), "Snapshot retained private fields"
        assert all(path in ("/trader/",WORKER_PATH) for _,_,path,_ in broker.requests), broker.requests
        if kind == "SharedWorker":
            await exercise_shared_markers(context, broker)
        print(f"PASS {kind} fresh snapshot races: no-change refresh, mismatch/retry, noise/late isolation, timeout cleanup, worker replacement/failure")
    finally:
        await context.close()
        await runner.cleanup()


async def exercise_shared_markers(context, broker):
    """Native SharedWorkers key subscriptions across every port of a client."""
    async def read_from_new_document():
        page = await context.new_page()
        try:
            await page.add_init_script(path=str(ROOT / "worker/tasks/portfolio_observer.js"))
            await page.goto(broker.origin + "/trader/")
            return await page.evaluate("""async path => {
              const worker = new SharedWorker(path,{name:'shared-marker-collision'});
              const port = worker.port;
              port.start();
              const ask = (action,requestId,data) => new Promise(resolve => {
                const receive = ({data}) => {
                  const message = JSON.parse(data);
                  if (message.data?.event === 'fixture:result' && message.data.data.requestId === requestId) {
                    port.removeEventListener('message',receive); resolve(message.data.data);
                  }
                };
                port.addEventListener('message',receive);
                port.postMessage(JSON.stringify({action,requestId,data}));
              });
              await ask('SUBSCRIBE_MESSAGE_PORT',1,{clientId:'9009',eventBrokerConfig:{
                currentClient:{id:9009,intAccount:123,accountInfo:{baseCurrency:'EUR'}},
              }});
              const snapshot = await window.__degitideReadPortfolio();
              const audit = await ask('fixture:stats',2);
              port.close();
              return {snapshot,audit};
            }""", WORKER_PATH, isolated_context=False)
        finally:
            await page.close()

    documents = await asyncio.gather(read_from_new_document(), read_from_new_document())
    markers = []
    for document in documents:
        assert not document["snapshot"].get("snapshot_error"), document
        audit = document["audit"]
        assert audit["active"] == 0 and len(audit["requests"]) == 4, document
        markers.append(audit["requests"][0]["params"]["degitideSnapshot"])
    assert len(set(markers)) == 2, "Independent documents reused the same native subscription key"


async def main():
    async with async_playwright() as playwright:
        browser = await playwright.chromium.launch(headless=True)
        try:
            for kind, scenario in (("Worker", "quoted"), ("SharedWorker", "quoted"), ("Worker", "failed-replay"),
                                   ("Worker", "fresh-snapshot"), ("SharedWorker", "fresh-snapshot"),
                                   ("Worker", "interleaved")):
                await exercise(browser, kind, scenario)
            for kind in ("Worker", "SharedWorker"):
                await exercise_lifecycle(browser, kind)
                await exercise_product_lookup(browser, kind)
                await exercise_snapshot_races(browser, kind)
        finally:
            await browser.close()


if __name__ == "__main__":
    asyncio.run(main())
