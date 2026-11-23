import json
import runpy
from datetime import date, timedelta
from pathlib import Path
from urllib.parse import urlsplit
from mitmproxy import ctx, http

addons = runpy.run_path('/integration/policy.py')['addons']
LOGIN = '''<form onsubmit="event.preventDefault(); document.querySelector('#otp').hidden=false">
<input name="username"><input type="password"><button type="submit">Sign in</button>
<input id="otp" hidden autocomplete="one-time-code" onkeydown="if(event.key==='Enter'){event.preventDefault();fetch('/login/secure/login/totp',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:document.querySelector('[name=username]').value})})}">
</form>'''
REPORT = '/portfolio-reports/secure/v4/transactions?intAccount=123&sessionId=fixture&fromDate=01%2F01%2F2020&toDate=31%2F12%2F2030&groupTransactionsByOrder=false'
PORTFOLIO = '/trading/secure/v5/update/123;jsessionid=fixture?portfolio=0&totalPortfolio=0&intAccount=123&sessionId=fixture'
SHELL = f'<html><body>Portfolio<script>fetch({json.dumps(REPORT)});fetch({json.dumps(PORTFOLIO)});</script></body></html>'


class Broker:
    def load(self, loader):
        ctx.options.update(upstream_cert=False)

    def request(self, flow):
        if flow.response is not None or flow.request.method == 'CONNECT':
            return
        path = urlsplit(flow.request.url).path
        status, content_type, body, headers = 200, 'application/json', {}, {}
        if path == '/login':
            content_type, body = 'text/html', LOGIN
        elif path == '/login/secure/login/totp':
            username = json.loads(flow.request.content)['username']
            headers['Set-Cookie'] = f'fixture_account={username}; Path=/; Secure; HttpOnly'
            body = {'status': 0}
        elif path == '/trader/':
            if 'fixture_account' not in flow.request.cookies:
                status, headers['Location'] = 302, '/login'
            else:
                content_type, body = 'text/html', SHELL
        elif '/v4/transactions' in path:
            body = {'transactions': [dict(id='trade-1', productId='100', symbol='Synthetic fund', currency='EUR', quantity=2, price=100, date=(date.today()-timedelta(days=1)).isoformat(), buySell='B')]}
        elif '/v5/update/' in path:
            scenario = Path('/data/scenario').read_text().strip() if Path('/data/scenario').exists() else 'initial'
            if scenario == 'failed':
                status, body = 503, {'error': 'synthetic failure'}
            else:
                quantity = 3 if flow.request.cookies.get('fixture_account') == 'savings' else 2
                body = {'portfolio': [] if scenario == 'empty' else [dict(id='100', symbol='Synthetic fund', currency='EUR', size=quantity, value=quantity*100, breakEvenPrice=100)]}
        elif path == '/productsearch/secure/v1/products/info':
            body = {'data': {'100': {'id':'100','symbol':'Synthetic fund','currency':'EUR'}}}
        else:
            status = 404
        flow.response = http.Response.make(status, body.encode() if isinstance(body,str) else json.dumps(body).encode(), {'Content-Type':content_type, **headers})

addons.append(Broker())
