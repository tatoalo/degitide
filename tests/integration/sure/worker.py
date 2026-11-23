"""Deterministic broker responses; never contacts DEGIRO or reads real sessions."""
import json
import os
from datetime import date, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit, parse_qs

HISTORY_START = date.today() - timedelta(days=750)
STATE = Path('/state/scenario.json')
STATE.parent.mkdir(exist_ok=True)
if not STATE.exists():
    STATE.write_text(json.dumps({'scenario': 'initial'}))


def portfolio(account, scenario):
    day = (date.today() - timedelta(days=1)).isoformat()
    def holding(product, currency, qty, price):
        return dict(product_id=product, symbol='Synthetic ' + product,
                    currency=currency, quantity=qty, avg_price=price,
                    market_value=qty * price)
    def trade(id, product, currency, qty, price, side='BUY'):
        return dict(id=id, product_id=product, symbol='Synthetic ' + product,
                    currency=currency, quantity=qty, price=price, type=side,
                    date=day if side == 'SELL' or id == 'buy-a-again' else HISTORY_START.isoformat(), fee=1)
    # The same product and transaction ID across accounts exercises isolation.
    if account == 'savings':
        return [holding('fund-a', 'EUR', 3, 100)], [trade('buy-a', 'fund-a', 'EUR', 3, 100)]
    holdings = [holding('fund-a', 'EUR', 10, 100), holding('fund-b', 'EUR', 5, 40), holding('fund-us', 'USD', 2, 50)]
    trades = [trade('buy-a', 'fund-a', 'EUR', 10, 100), trade('buy-b', 'fund-b', 'EUR', 5, 40), trade('buy-us', 'fund-us', 'USD', 2, 50)]
    if scenario in ('partial', 'closed', 'empty', 'failed', 'reopened'):
        holdings[0] = holding('fund-a', 'EUR', 6, 100)
        trades.append(trade('sell-a-part', 'fund-a', 'EUR', 4, 100, 'SELL'))
    if scenario in ('closed', 'empty', 'reopened'):
        holdings = holdings[1:]
        trades.append(trade('sell-a-rest', 'fund-a', 'EUR', 6, 100, 'SELL'))
    if scenario in ('empty', 'reopened'):
        holdings = []
        trades.extend([trade('sell-b', 'fund-b', 'EUR', 5, 40, 'SELL'), trade('sell-us', 'fund-us', 'USD', 2, 50, 'SELL')])
    if scenario == 'reopened':
        holdings = [holding('fund-a', 'EUR', 2, 110)]
        trades.append(trade('buy-a-again', 'fund-a', 'EUR', 2, 110))
    return holdings, trades


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, body):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        path = urlsplit(self.path)
        if path.path == '/scenario':
            return self.reply(200, json.loads(STATE.read_text()))
        token = Path(os.environ['FIXTURE_TOKEN_FILE']).read_text().strip() if os.environ.get('FIXTURE_TOKEN_FILE') else 'integration-worker-token'
        if self.headers.get('Authorization') != 'Bearer ' + token:
            return self.reply(401, {'error': 'fixture token required'})
        if path.path == '/tasks/check-session':
            return self.reply(200, {'valid': True})
        return self.reply(404, {})

    def do_POST(self):
        path = urlsplit(self.path)
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))) or '{}')
        if path.path == '/scenario':
            if body.get('scenario') not in ('initial', 'partial', 'closed', 'empty', 'failed', 'reopened'):
                return self.reply(400, {'error': 'unknown scenario'})
            STATE.write_text(json.dumps(body))
            return self.reply(200, body)
        token = Path(os.environ['FIXTURE_TOKEN_FILE']).read_text().strip() if os.environ.get('FIXTURE_TOKEN_FILE') else 'integration-worker-token'
        if self.headers.get('Authorization') != 'Bearer ' + token:
            return self.reply(401, {'error': 'fixture token required'})
        scenario = json.loads(STATE.read_text())['scenario']
        account = parse_qs(path.query).get('account', ['default'])[0]
        holdings, trades = portfolio(account, scenario)
        if path.path == '/tasks/fetch-transactions':
            return self.reply(200, {'transactions': [trade for trade in trades
                if body.get('from', '0000') <= trade['date'] <= body.get('to', '9999')]})
        if path.path == '/tasks/fetch-positions':
            snapshots = []
            for requested in body['dates']:
                day = date.fromisoformat(requested)
                weeks = (day - HISTORY_START).days // 7
                value = 100 + weeks * 2 if account == 'savings' else 500 + weeks * 5
                snapshots.append({'date': requested, 'base_currency': 'EUR', 'positions': [{
                    'identifier': 'FUND-A', 'name': 'Synthetic historic fund',
                    'quantity': 1, 'close': value, 'currency': 'EUR',
                    'value': value, 'base_value': value,
                }]})
            return self.reply(200, {'snapshots': snapshots, 'unavailable': []})
        if path.path == '/tasks/fetch-holdings':
            if scenario == 'failed' and account == 'default':
                return self.reply(503, {'error': 'synthetic holdings failure'})
            return self.reply(200, {'holdings': holdings})
        return self.reply(404, {})

    def log_message(self, fmt, *args):
        pass


ThreadingHTTPServer(('0.0.0.0', 9377), Handler).serve_forever()
