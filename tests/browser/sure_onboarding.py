"""Exercise the Sure setup UI with synthetic API responses, without broker access.

Run: .venv/bin/python tests/browser/sure_onboarding.py
Optional screenshots: SURE_SCREENSHOTS=/absolute/path/to/output
Requires the worker's Patchright dependency and its Chromium browser.
"""
import asyncio
import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import urlparse

from patchright.async_api import async_playwright, expect

STATIC = Path(__file__).resolve().parents[2] / 'internal/server/static'


async def exercise(browser, width):
    page = await browser.new_page(viewport={'width': width, 'height': 900})
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    now = datetime.now(timezone.utc)
    connection = {
        'id': 'synthetic-connection', 'name': 'Sure',
        'expires_at': (now + timedelta(minutes=30)).isoformat(),
        'claimed_at': None, 'last_used_at': None, 'revoked_at': None,
    }
    requests = []
    failure = {'create': False, 'check': False}
    created = False

    async def route_request(route):
        nonlocal created
        path = urlparse(route.request.url).path
        if path.startswith('/api/'):
            status = 200
            body = []
            if path == '/api/accounts':
                body = [{'id': 'default', 'name': 'Main account', 'connected': True},
                        {'id': 'savings', 'name': 'Savings', 'connected': True}]
            elif path == '/api/status':
                body = {'connected': True}
            elif path == '/api/versions':
                body = {'app': 'dev'}
            elif path == '/api/integrations/sure':
                if route.request.method == 'POST':
                    requests.append(route.request.post_data_json)
                    if failure['create']:
                        status, body = 400, {'error': 'Enable the private Docker network first.'}
                    else:
                        created = True
                        connection['base_url'] = requests[-1]['base_url']
                        connection['account_ids'] = requests[-1]['account_ids'] or ['default', 'savings']
                        body = dict(connection, setup_token='synthetic-token-do-not-use')
                elif failure['check']:
                    status, body = 503, {'error': 'Temporarily unavailable.'}
                else:
                    body = [dict(connection)] if created else []
            await route.fulfill(status=status, content_type='application/json', body=json.dumps(body))
        else:
            target = STATIC / (path.lstrip('/') or 'index.html')
            if target.is_file():
                await route.fulfill(path=str(target))
            else:
                await route.fulfill(status=404, body='Not found')

    await page.route('**/*', route_request)
    await page.goto('http://127.0.0.1:8391/')
    await expect(page.locator('#accountSelect option')).to_have_count(2)
    await page.locator('#appNav [data-view="accounts"]').click()
    await page.locator('#connectSureButton').click()
    await expect(page.locator('#sureBaseURL')).to_have_value('http://degitide-sure:8391')
    await expect(page.locator('[data-sure-progress="1"]')).to_have_attribute('aria-current', 'step')

    async def screenshot(name):
        assert await page.locator('#sureDialog').evaluate('(d) => d.scrollWidth <= d.clientWidth + 1')
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        if directory := os.environ.get('SURE_SCREENSHOTS'):
            Path(directory).mkdir(parents=True, exist_ok=True)
            await page.screenshot(path=str(Path(directory) / f'sure-{name}-{width}.png'))

    await screenshot('prepare')
    await page.locator('#sureServerSetup summary').click()
    await expect(page.locator('#sureDockerSetup')).to_be_visible()
    assert 'networks: [default, degitide]' in await page.locator('#sureDockerSetup').inner_text()
    await screenshot('server-setup')
    await page.locator('#sureServerSetup summary').click()
    await page.locator('[name="sureMethod"][value="https"]').check()
    await expect(page.locator('#sureBaseURL')).to_have_value('')
    await page.locator('#sureBaseURL').fill('https://portfolio.example.invalid')
    await page.locator('#sureAccount').select_option('savings')
    failure['create'] = True
    await page.locator('#createSureToken').click()
    await expect(page.locator('#sureError')).to_have_text('Enable the private Docker network first.')
    await expect(page.locator('#createSureToken')).to_be_enabled()
    failure['create'] = False
    await page.locator('#createSureToken').click()
    await expect(page.locator('[data-sure-step="2"]')).to_be_visible()
    assert requests[-1] == {'base_url': 'https://portfolio.example.invalid', 'account_ids': ['savings']}
    await expect(page.locator('#sureToken')).to_have_value('synthetic-token-do-not-use')
    await screenshot('connect')
    await page.add_script_tag(content="Object.defineProperty(navigator, 'clipboard', {configurable: true, value: {writeText: async () => {throw Error('denied')}}})")
    await page.locator('#copySureToken').click()
    await expect(page.locator('#copySureToken')).to_have_text('Select and copy token')
    assert await page.locator('#sureToken').evaluate('(e) => e.selectionEnd - e.selectionStart') == len('synthetic-token-do-not-use')
    await page.add_script_tag(content="Object.defineProperty(navigator, 'clipboard', {configurable: true, value: {writeText: async (text) => document.body.dataset.copiedToken = text}})")
    await page.locator('#copySureToken').click()
    await expect(page.locator('#copySureToken')).to_have_text('Copied')
    assert await page.locator('body').get_attribute('data-copied-token') == 'synthetic-token-do-not-use'
    await page.locator('#checkSureConnection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('Waiting for Sure to accept the token')
    await expect(page.locator('#finishSureSetup')).to_be_hidden()
    connection['claimed_at'] = now.isoformat()
    await page.locator('#refreshSureConnection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('Token accepted. Waiting for the first read.')
    await expect(page.locator('#finishSureSetup')).to_be_hidden()
    connection['last_used_at'] = now.isoformat()
    await page.locator('#refreshSureConnection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('Sure reached Degitide')
    await expect(page.locator('#finishSureSetup')).to_be_visible()
    await screenshot('check')
    await page.locator('#finishSureSetup').click()
    await expect(page.locator('#sureToken')).to_have_value('')
    await page.locator('#sureConnections').get_by_role('button', name='Check connection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('Sure reached Degitide')
    await expect(page.locator('#backSureToken')).to_be_hidden()
    failure['check'] = True
    await page.locator('#refreshSureConnection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('Couldn’t check the connection')
    await expect(page.locator('#finishSureSetup')).to_be_hidden()
    await expect(page.locator('#refreshSureConnection')).to_be_enabled()
    failure['check'] = False
    connection['revoked_at'] = now.isoformat()
    await page.locator('#refreshSureConnection').click()
    await expect(page.locator('#sureCheckTitle')).to_have_text('This connection was revoked')
    await page.locator('#restartSureSetup').click()
    await expect(page.locator('[data-sure-step="1"]')).to_be_visible()
    await expect(page.locator('#sureBaseURL')).to_have_value('https://portfolio.example.invalid')
    connection.update(claimed_at=None, last_used_at=None, revoked_at=None,
                      expires_at=(now - timedelta(minutes=1)).isoformat())
    await page.locator('#createSureToken').click()
    await page.locator('#checkSureConnection').click()
    assert requests[-1]['account_ids'] == ['savings'], 'Replacing a scoped token must not share all accounts'
    await expect(page.locator('#sureCheckTitle')).to_have_text('This token is no longer available')
    await expect(page.locator('#sureToken')).to_have_value('')
    await expect(page.locator('#backSureToken')).to_be_hidden()
    await page.keyboard.press('Escape')
    await expect(page.locator('#sureDialog')).not_to_be_visible()
    await page.goto('https://portfolio.example.invalid/')
    await expect(page.locator('#accountSelect option')).to_have_count(2)
    await page.locator('#appNav [data-view="accounts"]').click()
    await page.locator('#connectSureButton').click()
    await expect(page.locator('[name="sureMethod"][value="https"]')).to_be_checked()
    await expect(page.locator('#sureBaseURL')).to_have_value('https://portfolio.example.invalid')
    assert not errors, errors
    await page.close()
    print(f'PASS Sure onboarding at {width}px: connection choices, scoped token, copy/fallback, pending/claimed/read/error/revoked/expired states, resume, secret cleanup, HTTPS default and layout')


async def main():
    async with async_playwright() as pw:
        browser = await pw.chromium.launch(headless=True)
        try:
            for width in (1200, 390):
                await exercise(browser, width)
        finally:
            await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
