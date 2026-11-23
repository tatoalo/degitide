#!/usr/bin/env python3
import hashlib
import json
import os
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

version, owner = sys.argv[1:]
root = Path('dist/release')
expected = {f'degitide_{version}_{target}.tar.gz' for target in ('docker', 'linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64')}
entries = dict(line.split('  ', 1)[::-1] for line in (root / 'SHA256SUMS').read_text().splitlines())
assert set(entries) == expected, 'missing or unexpected release assets'
for name, digest in entries.items():
    with (root / name).open('rb') as file:
        assert hashlib.file_digest(file, 'sha256').hexdigest() == digest, name
    with tarfile.open(root / name) as archive:
        if name.endswith('_docker.tar.gz'):
            text = archive.extractfile('degitide/docker-compose.yml').read().decode()
            assert archive.getmember('degitide/integrations/sure/degitide_simplefin.rb').isfile()
            assert archive.getmember('degitide/docker-compose.sure.yml').isfile()
            sure_override = archive.extractfile('degitide/docker-compose.sure.yml').read()
            manifest = json.loads(text)
            split_text = archive.extractfile('degitide/docker-compose.split.yml').read().decode()
            split = json.loads(split_text)
            login_text = archive.extractfile('degitide/docker-compose.login.yml').read().decode()
            assert manifest['name'] == 'degitide'
            env_example = archive.extractfile('degitide/.env.example').read().decode()
            assert 'DEGITIDE_SCHEDULE=market\n' in env_example
            assert 'DEGITIDE_SYNC_INTERVAL_MINUTES=60\n' in env_example
            assert 'DEGITIDE_MAINTENANCE_SCHEDULE="CRON_TZ=Europe/Dublin 0 2 * * *"' in env_example
            assert '@VERSION@' not in archive.extractfile('degitide/README.md').read().decode()
        else:
            assert set(archive.getnames()) == {'degitide', 'README.md'}
            assert archive.getmember('degitide').mode & 0o111, 'CLI is not executable'
            header = archive.extractfile('degitide').read(20)
            if '_linux_' in name:
                assert header[:4] == b'\x7fELF'
                assert int.from_bytes(header[18:20], 'little') == (183 if '_arm64' in name else 62)
            else:
                assert header[:4] == b'\xcf\xfa\xed\xfe'
                assert int.from_bytes(header[4:8], 'little') == (0x100000c if '_arm64' in name else 0x1000007)
services = manifest['services']
assert set(services) == {'app'}
assert set(manifest['volumes']) == {'degitide'}
assert len(services['app']['volumes']) == 1
assert len(services['app']['ports']) == 1
assert services['app']['image'] == f'ghcr.io/{owner.lower()}/degitide/server:{version[1:]}'
assert len({service['image'] for service in split['services'].values()}) == 5
for service in [*services.values(), *split['services'].values()]:
    assert 'build' not in service
    assert service['image'].startswith(f'ghcr.io/{owner.lower()}/degitide/')
    assert service['image'].endswith(':' + version[1:])
    for port in service.get('ports', []):
        assert port['host_ip'] == '127.0.0.1'
for app in (services['app'], split['services']['app']):
    assert app['environment']['DEGITIDE_SCHEDULE'] == '${DEGITIDE_SCHEDULE:-market}'
    assert app['environment']['DEGITIDE_SYNC_INTERVAL_MINUTES'] == '${DEGITIDE_SYNC_INTERVAL_MINUTES:-60}'
assert services['app']['environment']['DEGITIDE_TELEGRAM_TOKEN'] == '${DEGITIDE_TELEGRAM_TOKEN:-}'
with tempfile.TemporaryDirectory() as directory:
    compose = Path(directory) / 'compose.yaml'
    compose.write_text(text)
    env = {**os.environ, 'DEGITIDE_SCHEDULE': '0 8 * * *', 'DEGITIDE_NOTIFY_PROVIDER': 'telegram'}
    configured = json.loads(subprocess.check_output([
        'docker', 'compose', '--env-file', '/dev/null', '--profile', 'notifications',
        '-f', str(compose), 'config', '--format', 'json',
    ], text=True, env=env))
    assert configured['services']['app']['environment']['DEGITIDE_SCHEDULE'] == '0 8 * * *'
    assert configured['services']['app']['environment']['DEGITIDE_NOTIFY_PROVIDER'] == 'telegram'
    assert configured['volumes']['degitide']['name'] == 'degitide_degitide'
    split_file = Path(directory) / 'docker-compose.split.yml'
    split_file.write_text(split_text)
    subprocess.run(['docker', 'compose', '--env-file', '/dev/null', '-f', str(split_file), 'config', '--quiet'], check=True)
    login_file = Path(directory) / 'docker-compose.login.yml'
    login_file.write_text(login_text)
    native = json.loads(subprocess.check_output(['docker', 'compose', '--env-file', '/dev/null', '-f', str(split_file), '-f', str(login_file), '--profile', 'native-login', 'config', '--format', 'json'], text=True))
    assert native['services']['native-login']['image'] == f'ghcr.io/{owner.lower()}/degitide/worker:{version[1:]}'
    override = Path(directory) / 'docker-compose.sure.yml'
    override.write_bytes(sure_override)
    subprocess.run(['docker', 'compose', '--env-file', '/dev/null', '-f', str(compose),
        '-f', str(override), 'config', '--quiet'], check=True, env=env)
print('Release assets verified: checksums, native formats, pinned images, loopback access and configurable services.')
