import argparse
import http.cookiejar
import json
import os
import re
import subprocess
import time
from pathlib import Path
from urllib.parse import urlencode
from urllib.request import Request, build_opener, HTTPCookieProcessor

ROOT = Path(__file__).resolve().parents[3]
COMPOSE = ['docker', 'compose', '--env-file', '/dev/null', '-p', 'degitide-aio-e2e', '-f', str(ROOT/'docker-compose.yml'), '-f', str(Path(__file__).with_name('compose.yml'))]
PASSWORD = 'Synthetic-integration-2026!'


def compose(*args, stdin=None, check=True):
    result = subprocess.run(COMPOSE + list(args), input=stdin, text=True, capture_output=True, timeout=600)
    if check and result.returncode:
        raise RuntimeError(result.stdout[-1500:] + result.stderr[-2500:])
    return result


def cli(*args, data=None, check=True):
    return compose('exec', '-T', 'app', 'degitide', *args, stdin=json.dumps(data) if data else None, check=check)


def inside(script):
    return compose('exec', '-T', 'app', 'python', '-c', script).stdout.strip()


def scenario(value):
    inside(f"from pathlib import Path; Path('/data/scenario').write_text({value!r})")


def secrets_digest():
    return inside("import hashlib,pathlib,json; print(json.dumps({p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in pathlib.Path('/data/secrets').iterdir()},sort_keys=True))")


def main(*, build=True):
    compose('down', '--volumes', '--remove-orphans')
    compose('up', '-d', '--build' if build else '--no-build', '--wait', '--wait-timeout', '120')
    container = compose('ps', '-q', 'app').stdout.strip()
    info = json.loads(subprocess.check_output(['docker','inspect',container]))[0]
    assert len([m for m in info['Mounts'] if m['Type']=='volume']) == 1
    assert set(info['Config']['ExposedPorts']) == {'8391/tcp'}
    assert set(info['HostConfig']['PortBindings']) == {'8391/tcp'}
    assert info['Config']['User'] == 'degitide-worker'
    inside("import socket; ip=socket.gethostbyname(socket.gethostname()); assert all(socket.socket().connect_ex((ip,p)) != 0 for p in (8080,9377,9380))")
    base = 'http://' + compose('port','app','8391').stdout.strip()
    opener = build_opener(HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(path):
        return opener.open(base+path,timeout=10).read().decode()
    def api(path):
        return json.loads(get(path))
    def login():
        html = get('/login')
        csrf = re.search(r'name="csrf_token"[^>]*value="([^"]+)"',html).group(1)
        request = Request(base+'/login',data=urlencode({'username':'integration','password':PASSWORD,'csrf_token':csrf}).encode(),headers={'Origin':base})
        assert opener.open(request,timeout=10).status == 200
    assert 'setup' in opener.open(base).url
    cli('setup','--credentials-stdin',data={'username':'integration','password':PASSWORD})
    login()
    cli('accounts','add','--name','Savings','--id','savings')
    for account in ('default','savings'):
        cli('connect','--account',account,'--credentials-stdin',data={'username':account,'password':'synthetic-password','seed':'JBSWY3DPEHPK3PXP'})
    assert base in cli('open','--url',base,'--no-open').stdout
    cli('sync','--timeout','90s')
    before = {account:api('/api/holdings?account='+account) for account in ('default','savings')}
    assert before['default'][0]['quantity'] == 2
    assert before['savings'][0]['quantity'] == 3
    for format in ('csv','pdf'):
        with opener.open(base+'/api/transactions/export?format='+format) as response:
            assert response.status == 200
            assert response.read()
    secret_state = secrets_digest()
    inside("from pathlib import Path; paths=list(Path('/data/browser-profile').rglob('credentials.enc')); assert len(paths)==2 and all(p.read_bytes().startswith(b'gAAAA') for p in paths)")
    print('PASS one volume/port, loopback services, CLI setup/connect, browser sign-in, multi-account sync and exports',flush=True)
    scenario('failed')
    assert cli('sync','--timeout','90s',check=False).returncode != 0
    assert api('/api/holdings?account=default') == before['default']
    scenario('empty')
    cli('sync','--timeout','90s')
    assert api('/api/holdings?account=default') == []
    scenario('initial')
    cli('sync','--timeout','90s')
    for operation in [('restart',), ('up','-d','--force-recreate','--wait','--wait-timeout','120')]:
        compose(*operation)
        compose('up','-d','--wait','--wait-timeout','120')
        base = 'http://' + compose('port','app','8391').stdout.strip()
        login()
        assert secrets_digest() == secret_state
        cli('sync','--timeout','90s')
        for account in ('default','savings'):
            assert api('/api/holdings?account='+account) == before[account]
    print('PASS failure preservation, closed/reopened positions, restart and recreation with saved encrypted sessions',flush=True)
    os.environ['AIO_TEST_SCHEDULE'] = '@every 10s'
    compose('up','-d','--force-recreate','--wait','--wait-timeout','120')
    base = 'http://' + compose('port','app','8391').stdout.strip()
    login()
    previous = api('/api/status')['last_pull']
    deadline = time.monotonic()+90
    while api('/api/status')['last_pull'] == previous:
        assert time.monotonic() < deadline, 'scheduled sync did not run'
        time.sleep(2)
    print('PASS scheduled sync without UI or CLI',flush=True)
    os.environ.pop('AIO_TEST_SCHEDULE')
    compose('up','-d','--force-recreate','--wait','--wait-timeout','120')
    inside("import os,signal; from pathlib import Path; p=next(p for p in Path('/proc').iterdir() if p.name.isdigit() and p.name!=str(os.getpid()) and p.joinpath('cmdline').read_bytes().startswith(b'python\\x00/app/worker/main.py')); os.kill(int(p.name),signal.SIGKILL)")
    deadline = time.monotonic()+90
    while True:
        info = json.loads(subprocess.check_output(['docker','inspect',compose('ps','-aq','app').stdout.strip()]))[0]
        if info['RestartCount'] > 0 and info['State'].get('Health',{}).get('Status') == 'healthy':
            break
        assert time.monotonic() < deadline, 'container did not recover after worker crash'
        time.sleep(1)
    assert secrets_digest() == secret_state
    cli('sync','--timeout','90s')
    print('PASS child crash restarts the whole container and preserves data',flush=True)
    compose('stop','--timeout','45')
    info = json.loads(subprocess.check_output(['docker','inspect',compose('ps','-aq','app').stdout.strip()]))[0]
    assert info['State']['ExitCode'] == 0, info['State']
    print('PASS graceful shutdown',flush=True)
    name = 'degitide-aio-notify-e2e'
    subprocess.run(['docker','run','-d','--name',name,'--network','none',
        '-e','DEGITIDE_NOTIFY_PROVIDER=pushover','-e','DEGITIDE_PUSHOVER_TOKEN=synthetic',
        '-e','DEGITIDE_PUSHOVER_USER=synthetic','degitide-all-in-one:local','serve'],check=True,capture_output=True)
    try:
        deadline = time.monotonic()+60
        while True:
            info = json.loads(subprocess.check_output(['docker','inspect',name]))[0]
            if info['State'].get('Health',{}).get('Status') == 'healthy':
                break
            assert time.monotonic() < deadline, 'optional notifier did not start'
            time.sleep(1)
        subprocess.run(['docker','exec',name,'python','-c',
            "import socket; assert socket.create_connection(('127.0.0.1',9380),timeout=2)"],check=True)
    finally:
        subprocess.run(['docker','stop','--time','45',name],check=True,capture_output=True)
        subprocess.run(['docker','rm','-v',name],check=True,capture_output=True)
    print('PASS optional notifier inside the same container without external traffic',flush=True)



if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--no-build', action='store_true', help='Use the image already built and loaded by CI')
    args = parser.parse_args()
    main(build=not args.no_build)
