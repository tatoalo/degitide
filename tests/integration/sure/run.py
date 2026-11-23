#!/usr/bin/env python3
"""Exercise the real Sure web/controllers/jobs and Degitide scheduler over HTTP."""
import argparse
import base64
import hashlib
import http.cookiejar
import json
import re
import subprocess
import tempfile
import time
from datetime import datetime, timezone
from decimal import Decimal
from html.parser import HTMLParser
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, build_opener, HTTPCookieProcessor
from runner import SureRunner

HERE = Path(__file__).resolve().parent
COMPOSE = ['docker', 'compose', '-f', str(HERE / 'compose.yml'), '-f', str(HERE / 'compat.yml')]
EMAIL, PASSWORD = 'integration@example.invalid', 'Sure-integration-2026!'
RUNNER = SureRunner(COMPOSE)
INCLUDE_TRANSACTIONS = False


def compose(*args, stdin=None):
    result = subprocess.run(COMPOSE + list(args), input=stdin, text=True, capture_output=True, timeout=180)
    if result.returncode:
        raise RuntimeError(f'compose {args[:3]} failed: {result.stderr[-2000:]} {result.stdout[-2000:]}')
    return result.stdout.strip()


def sql(query):
    return compose('exec', '-T', 'db', 'psql', '-U', 'sure_test', '-d', 'sure_test', '-tAc', query)


def service_url(service, port):
    return 'http://' + compose('port', service, str(port))


class HTML(HTMLParser):
    def __init__(self, body):
        super().__init__()
        self.csrf, self.user_path = None, None
        self.feed(body)
    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == 'meta' and attrs.get('name') == 'csrf-token':
            self.csrf = attrs.get('content')
        if tag == 'form' and re.fullmatch('/users/[a-f0-9-]+', attrs.get('action', '')):
            self.user_path = attrs['action']


class Client:
    def __init__(self, base, auth=None):
        self.base, self.auth = base, auth
        self.opener = build_opener(HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def request(self, path, body=None, method=None, form=False):
        headers = {'Origin': self.base, 'Accept': 'text/html' if form else 'application/json'}
        if self.auth:
            headers['Authorization'] = 'Basic ' + base64.b64encode(self.auth.encode()).decode()
        if body is not None:
            headers['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
            body = (urlencode(body) if form else json.dumps(body)).encode()
        request = Request(self.base + path, data=body, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=45) as response:
                return response.status, response.read().decode(), response.url
        except HTTPError as error:
            return error.code, error.read().decode(), error.url
    def json(self, path, body=None, method=None):
        status, text, _ = self.request(path, body, method)
        assert status < 300, f'{path}: HTTP {status}: {text[:200]}'
        return json.loads(text)
    def form(self, page, target, fields):
        status, body, _ = self.request(page, form=True)
        assert status == 200, f'Cannot load {page}: {status}'
        csrf = HTML(body).csrf
        assert csrf, f'No CSRF on {page}'
        fields = {'authenticity_token': csrf, **fields}
        status, body, url = self.request(target, fields, form=True)
        assert status < 300, f'{target}: HTTP {status}'
        return body, url


def snapshot():
    return RUNNER.request('snapshot')


def wait_jobs():
    deadline = time.monotonic() + 90
    quiet = 0
    while time.monotonic() < deadline:
        jobs = RUNNER.request('idle')
        quiet = quiet + 1 if jobs['busy'] == 0 and jobs['queued'] == 0 else 0
        if quiet >= 3:
            return
        time.sleep(1)
    raise AssertionError('Sure jobs did not settle in 90 seconds')


def setup(degitide, sure):
    if degitide.request('/api/accounts')[0] == 503:
        compose('exec', '-T', 'degitide', 'degitide', 'setup', '--credentials-stdin', stdin=json.dumps({'username':'integration','password':PASSWORD}))
    accounts = degitide.json('/api/accounts')
    if not any(a['id'] == 'savings' for a in accounts):
        degitide.json('/api/accounts', {'id':'savings','name':'Synthetic savings'})
    if int(sql('SELECT count(*) FROM users')) == 0:
        sure.form('/registration/new', '/registration', {'user[email]':EMAIL,'user[password]':PASSWORD,'user[password_confirmation]':PASSWORD})
    else:
        sure.form('/sessions/new', '/sessions', {'email':EMAIL,'password':PASSWORD})
    status, body, _ = sure.request('/onboarding', form=True)
    doc = HTML(body)
    assert doc.user_path, 'Sure login/onboarding failed'
    sure.form('/onboarding',doc.user_path,{'_method':'patch','user[first_name]':'Synthetic','user[last_name]':'Tester','user[onboarded_at]':datetime.now(timezone.utc).isoformat(),'user[redirect_to]':'home','user[family_attributes][name]':'Integration only','user[family_attributes][currency]':'EUR','user[family_attributes][country]':'IE'})


def connect(degitide, sure):
    state = snapshot()
    if not state['items']:
        grant = degitide.json('/api/integrations/sure', {'base_url':'http://degitide-sure:8391','name':'Sure E2E','include_transactions':INCLUDE_TRANSACTIONS})
        body, url = sure.form('/simplefin_items/new', '/simplefin_items', {'simplefin_item[setup_token]':grant['setup_token']})
        assert '/accounts' in url, 'Sure failed to claim the connection'
        wait_jobs()
        state = snapshot()
    assert len(state['items']) == 1, 'Expected exactly one Sure connection'
    item = state['items'][0]['id']
    unlinked = [a for a in state['accounts'] if not a['account_id']]
    if unlinked:
        fields = {}
        for account in unlinked:
            fields[f"account_types[{account['provider_id']}]"] = 'Investment'
            fields[f"account_subtypes[{account['provider_id']}]"] = 'brokerage'
        sure.form(f'/simplefin_items/{item}/setup_accounts',f'/simplefin_items/{item}/complete_account_setup',fields)
        wait_jobs()
    return item


def sync_sure(sure, item, allow_failure=False):
    before = sql('SELECT count(*) FROM syncs')
    sure.form('/accounts', f'/simplefin_items/{item}/sync', {})
    assert sql('SELECT count(*) FROM syncs') != before, 'Sync HTTP action did not schedule work'
    wait_jobs()
    state = snapshot()
    if not allow_failure:
        runs = [s for s in state['syncs'][int(before):] if s['type']=='SimplefinItem']
        assert runs and all(s['status']=='completed' for s in runs), 'Sure provider sync failed'
        assert all(i['status']=='good' for i in state['items']), 'Sure connection requires attention'
    return state


def ticker(product):
    return 'CUSTOM:DEGIRO_' + hashlib.sha256(product.encode()).hexdigest()[:32].upper()


def new_syncs(before, after):
    previous = {s['id'] for s in before['syncs']}
    return [s for s in after['syncs'] if s['id'] not in previous]


def assert_scheduled_sync(state):
    # Invoke the same job registered by Sure's recurring scheduler, through
    # real Sidekiq. This verifies the unattended path, without either UI.
    RUNNER.request('schedule')
    wait_jobs()
    scheduled = snapshot()
    runs = new_syncs(state,scheduled)
    for kind in ('Family','SimplefinItem'):
        assert any(s['type']==kind and s['status']=='completed' for s in runs), f'{kind} scheduled sync did not complete'
    assert not any(s['status']=='failed' for s in runs), 'scheduled sync failed'
    assert scheduled['accounts'] == state['accounts'], 'scheduled sync changed persisted records'
    print('PASS unattended Sure scheduled-job path without UI',flush=True)
    return scheduled


def assert_holdings_pages(sure, state):
    for account in state['accounts']:
        id = account['account_id']
        status, body, _ = sure.request('/holdings?account_id='+id, form=True)
        assert status == 200 and f'id="holdings_account_{id}"' in body, 'Sure holdings page failed to render'
        rendered = set(re.findall(r'<turbo-frame id="holding_([a-f0-9-]+)"', body))
        assert rendered == {h['id'] for h in account['current']}, 'Sure rendered stale or missing positions'


def assert_portfolio(state, scenario):
    expected = {
        'initial': {'fund-a':10,'fund-b':5},
        'partial': {'fund-a':6,'fund-b':5},
        'closed': {'fund-b':5},
        'empty': {},
        'reopened': {'fund-a':2},
    }[scenario]
    accounts = {a['source_id']: a for a in state['accounts']}
    assert len(accounts) == 3, f'Expected 3 broker/currency accounts, got {list(accounts)}'
    eur = accounts['degitide-default-EUR']
    actual = {h['ticker']: Decimal(h['qty']) for h in eur['current']}
    assert actual == {ticker(k):Decimal(v) for k,v in expected.items()}, f'{scenario}: Sure current positions {actual}, expected {expected}'
    savings = accounts['degitide-savings-EUR']
    assert {h['ticker']:Decimal(h['qty']) for h in savings['current']} == {ticker('fund-a'):Decimal(3)}, 'Second account was modified'
    usd = accounts['degitide-default-USD']
    expected_usd = {} if scenario in ('empty','reopened') else {ticker('fund-us'):Decimal(2)}
    assert {h['ticker']:Decimal(h['qty']) for h in usd['current']} == expected_usd, 'USD positions incorrect'
    for account in accounts.values():
        total = sum(Decimal(h['amount']) for h in account['current'])
        assert total == Decimal(account['balance']), f'{scenario}: {account["source_id"]} balance disagrees with holdings'
        assert Decimal(account['cash']) == 0, f'{scenario}: invented cash in {account["source_id"]}'
        ids = [e['external_id'] for e in account['entries'] if e['external_id']]
        assert len(ids) == len(set(ids)), 'Duplicate external transaction IDs'
    if not INCLUDE_TRANSACTIONS:
        assert all(e['type'] != 'Transaction' for a in accounts.values() for e in a['entries']), 'Purchases were imported as ordinary spending'
    if scenario == 'reopened' and INCLUDE_TRANSACTIONS:
        # Independently check the complete ledger, including the opposite sign
        # convention Sure uses. Fees must not be folded into gross consideration.
        expected_entries = {
            'default-EUR': {'buy-a':1000, 'buy-b':200, 'sell-a-part':-400,
                            'sell-a-rest':-600, 'sell-b':-200, 'buy-a-again':220},
            'default-USD': {'buy-us':100, 'sell-us':-100},
            'savings-EUR': {'buy-a':300},
        }
        for source, trades in expected_entries.items():
            account_id = source.rsplit('-',1)[0]
            expected = {'simplefin_degitide-' + hashlib.sha256((account_id+'\0'+id).encode()).hexdigest()[:32]:Decimal(amount) for id,amount in trades.items()}
            actual = {e['external_id']:Decimal(e['amount']) for e in accounts['degitide-'+source]['entries'] if e['type']=='Transaction'}
            assert actual == expected, f'{source}: missing or incorrect transaction activity'


def assert_history(state):
    for account in state['accounts']:
        history = [row for row in account['history'] if row['source'] == 'report']
        if not history:
            continue  # The synthetic USD account has only a current snapshot.
        assert len(history) > 100, 'Saved historical report observations were not exported'
        assert len({row['balance'] for row in history}) > 1, 'Historical feed is artificially flat'
        actual = {entry['date']:Decimal(entry['amount']) for entry in account['valuations'] if entry['source']=='degitide_valuation'}
        materialized = {row['date']:Decimal(row['amount']) for row in account['historical_balances']}
        for row in history:
            expected = Decimal(row['balance'])
            assert actual.get(row['date']) == expected, f"Missing/wrong native valuation on {row['date']}"
            assert materialized.get(row['date']) == expected, f"Sure chart balance differs from report on {row['date']}"
        assert account['history_start'] <= history[0]['date'], 'Sure trimmed supported history to the latest holdings'
        if not INCLUDE_TRANSACTIONS:
            anchor = next(entry for entry in account['valuations'] if entry['kind']=='opening_anchor')
            assert anchor['date'] == history[0]['date'] and anchor['source']=='degitide_valuation', 'Synthetic two-year opening was not replaced by a real observation'
    print('PASS historical valuations: full observed history, chart balances, stable opening and no invented activity',flush=True)


def main():
    global INCLUDE_TRANSACTIONS
    parser = argparse.ArgumentParser()
    parser.add_argument('--all-in-one', action='store_true')
    parser.add_argument('--with-transactions', action='store_true', help='test the optional ordinary-ledger activity mode')
    parser.add_argument('--scenario', choices=['initial','partial','closed','empty','reopened'])
    parser.add_argument('--recovery-only', action='store_true', help='rerun recovery checks after an initial successful portfolio run')
    parser.add_argument('--stock', action='store_true', help='exercise unmodified Sure (expected to expose closed-position bug)')
    args = parser.parse_args()
    INCLUDE_TRANSACTIONS = args.with_transactions
    if args.stock:
        del COMPOSE[-2:]
    if args.all_in_one:
        COMPOSE.extend(['-f', str(HERE/'all-in-one.yml'), '-p', 'degitide-sure-aio'])
    artifacts = Path(tempfile.mkdtemp(prefix='degitide-sure-e2e-'))
    degitide = Client(service_url('degitide',8391),'degitide:integration-dashboard-token')
    sure = Client(service_url('web',3000))
    fixture = Client(service_url('broker-fixture',9377))
    setup(degitide,sure)
    scenarios = [args.scenario] if args.scenario else ['reopened'] if args.recovery_only else ['initial','partial','closed','empty','reopened']
    print(f'Artifacts: {artifacts}', flush=True)
    item = None
    for scenario in scenarios:
        fixture.json('/scenario', {'scenario':scenario})
        compose('exec','-T','degitide','degitide','sync','--timeout','30s')
        if item is None:
            item = connect(degitide,sure)
        state = sync_sure(sure,item)
        (artifacts / f'{scenario}.json').write_text(json.dumps(state,indent=2))
        assert_portfolio(state,scenario)
        if not args.stock:
            assert_history(state)
        assert_holdings_pages(sure,state)
        print(f'PASS {scenario}: actual Sure holdings, balances, currencies and account isolation',flush=True)
    if not args.scenario:
        # Every already-imported transaction must keep its Sure database ID.
        before = state
        again = sync_sure(sure,item)
        assert again['accounts'] == before['accounts'], 'repeat sync changed persisted identities or values'
        print('PASS repeat sync: no duplicate holdings or transactions',flush=True)

        # A failed broker pull must preserve good Sure data, including other accounts.
        fixture.json('/scenario', {'scenario':'failed'})
        status = degitide.json('/api/pull/trigger', {})
        deadline = time.monotonic()+30
        while degitide.json('/api/status')['pull_running']:
            assert time.monotonic()<deadline, 'broker failure did not finish'
            time.sleep(.2)
        failed = sync_sure(sure,item)
        assert failed['accounts'] == again['accounts'], 'failed account lost or changed imported data'
        assert any((s.get('stats') or {}).get('errors') for s in new_syncs(again,failed)), 'Sure did not surface the partial failure'
        print('PASS partial broker failure: previous positions retained and error visible in Sure',flush=True)
        fixture.json('/scenario', {'scenario':'reopened'})
        compose('exec','-T','degitide','degitide','sync','--timeout','30s')
        state = sync_sure(sure,item)
        assert_portfolio(state,'reopened')
        state = assert_scheduled_sync(state)

        # Queue a real sync while Sidekiq is offline, then recover it.
        compose('stop','worker')
        sure.form('/accounts',f'/simplefin_items/{item}/sync',{})
        assert int(sql("SELECT count(*) FROM syncs WHERE status='pending'")) > 0, 'job not durably queued'
        compose('start','worker')
        wait_jobs()
        assert snapshot()['accounts'] == state['accounts'], 'worker restart replayed data incorrectly'
        print('PASS queued-job recovery after worker restart',flush=True)

        # Restart every service, including both databases, with persisted volumes.
        before_restart = snapshot()
        RUNNER.close()
        compose('restart')
        # Docker may reassign ephemeral host ports when restarting containers.
        # The internal provider URL remains stable; retain the browser cookies.
        degitide.base = service_url('degitide',8391)
        sure.base = service_url('web',3000)
        fixture.base = service_url('broker-fixture',9377)
        for client,path in [(degitide,'/healthz'),(sure,'/up')]:
            deadline = time.monotonic()+90
            while True:
                try:
                    if client.request(path)[0] == 200: break
                except OSError: pass
                assert time.monotonic()<deadline, f'{path} did not recover'
                time.sleep(1)
        wait_jobs()
        after_restart = sync_sure(sure,item)
        assert after_restart['accounts'] == before_restart['accounts'], 'restart changed identity, balances or positions'
        print('PASS complete stack restart: saved credentials and portfolio preserved',flush=True)

        # Revoke access and exercise Sure's actual reconnect controller/job.
        grants = degitide.json('/api/integrations/sure')
        for grant in grants:
            if grant['name']=='Sure E2E' and not grant['revoked_at']:
                degitide.json('/api/integrations/sure/'+grant['id'],{},method='DELETE')
        revoked = sync_sure(sure,item,allow_failure=True)
        assert revoked['accounts'] == after_restart['accounts'], 'revocation erased financial data'
        assert any(s['status']=='failed' for s in new_syncs(after_restart,revoked)), 'revocation was not surfaced as a failed sync'
        grant = degitide.json('/api/integrations/sure',{'base_url':'http://degitide-sure:8391','name':'Sure E2E','include_transactions':INCLUDE_TRANSACTIONS})
        sure.form(f'/simplefin_items/{item}/edit',f'/simplefin_items/{item}',{'_method':'patch','simplefin_item[setup_token]':grant['setup_token']})
        wait_jobs()
        refreshed = snapshot()
        assert len(refreshed['items'])==1, 'reconnect duplicated the connection'
        item = refreshed['items'][0]['id']
        reconnected = sync_sure(sure,item)
        assert reconnected['accounts'] == after_restart['accounts'], 'reconnect changed account/record IDs'
        print('PASS revocation and reconnect: access restored without duplication',flush=True)
        RUNNER.request('guards')
        (artifacts/'final.json').write_text(json.dumps(reconnected,indent=2))
    print(f'Sure: {sure.base}\nDegitide: {degitide.base}',flush=True)

if __name__ == '__main__':
    try:
        main()
    finally:
        RUNNER.close()
        print(f'Sure inspections: {RUNNER.requests} requests, {RUNNER.boots} Rails boots, {RUNNER.seconds:.1f}s', flush=True)
