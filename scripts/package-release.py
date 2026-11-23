#!/usr/bin/env python3
import argparse
import hashlib
import json
import re
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('version')
    parser.add_argument('owner')
    parser.add_argument('--validate', action='store_true')
    args = parser.parse_args()
    if not re.fullmatch(r'v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?', args.version):
        parser.error('use vX.Y.Z or vX.Y.Z-prerelease')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]*', args.owner):
        parser.error('invalid GitHub owner')
    if args.validate:
        return
    output = Path('dist/release')
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error('dist/release must be empty')
    readme = Path('docs/release-install.md').read_text().replace('@VERSION@', args.version).replace('@OWNER@', args.owner)
    manifests = {}
    for filename in ('docker-compose.yml', 'docker-compose.split.yml'):
        manifest = json.loads(subprocess.check_output([
            'docker', 'compose', '--project-name', 'degitide', '--profile', '*', '--env-file', '/dev/null',
            '-f', filename, 'config', '--no-interpolate', '--no-env-resolution',
            '--no-path-resolution', '--no-normalize', '--format', 'json',
        ], text=True))
        manifest['name'] = 'degitide'
        for service in manifest['services'].values():
            service.pop('build', None)
            image = re.fullmatch(r'\$\{DEGITIDE_(?:[A-Z_]+_)?IMAGE:-(degitide(?:-[a-z-]+)?)\}', service['image'])
            if image is None:
                raise ValueError('unexpected image reference: ' + service['image'])
            component = 'server' if image[1] == 'degitide' else image[1].removeprefix('degitide-')
            service['image'] = f'ghcr.io/{args.owner.lower()}/degitide/{component}:{args.version[1:]}'
        manifests[filename] = manifest
    with tempfile.TemporaryDirectory() as directory:
        bundle = Path(directory) / 'degitide'
        bundle.mkdir()
        for filename, manifest in manifests.items():
            (bundle / filename).write_text(json.dumps(manifest, indent=2) + '\n')
        (bundle / '.env.example').write_text('DEGITIDE_SCHEDULE=market\nDEGITIDE_SYNC_INTERVAL_MINUTES=60\nDEGITIDE_MAINTENANCE_SCHEDULE="CRON_TZ=Europe/Dublin 0 2 * * *"\nDEGITIDE_USE_PREBUILT=true\n')
        (bundle / 'README.md').write_text(readme)
        shutil.copy('docker-compose.sure.yml', bundle)
        login = Path('docker-compose.login.yml').read_text().replace('${DEGITIDE_WORKER_IMAGE:-degitide-worker}', f'ghcr.io/{args.owner.lower()}/degitide/worker:{args.version[1:]}')
        (bundle / 'docker-compose.login.yml').write_text(login)
        (bundle / 'docs').mkdir()
        shutil.copy('docs/split-deployment.md', bundle / 'docs')
        (bundle / 'scripts').mkdir()
        shutil.copy('scripts/native-login.sh', bundle / 'scripts')
        shutil.copytree('integrations/sure', bundle / 'integrations/sure')
        with tarfile.open(output / f'degitide_{args.version}_docker.tar.gz', 'w:gz') as archive:
            archive.add(bundle, arcname='degitide')
        for os in ('linux', 'darwin'):
            for arch in ('amd64', 'arm64'):
                with tarfile.open(output / f'degitide_{args.version}_{os}_{arch}.tar.gz', 'w:gz') as archive:
                    archive.add(f'dist/bin/{os}_{arch}/degitide', arcname='degitide')
                    archive.add(bundle / 'README.md', arcname='README.md')
    sums = []
    for path in sorted(output.glob('*.tar.gz')):
        with path.open("rb") as file:
            sums.append(f'{hashlib.file_digest(file, "sha256").hexdigest()}  {path.name}\n')
    (output / 'SHA256SUMS').write_text(''.join(sums))


if __name__ == '__main__':
    main()
