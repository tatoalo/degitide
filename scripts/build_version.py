#!/usr/bin/env python3
"""Resolve a readable image version from release tags and commit distance."""
import re
import subprocess


def build_version(cwd=None):
    description = subprocess.run(
        ['git', 'describe', '--tags', '--match', 'v[0-9]*', '--long'],
        cwd=cwd, text=True, capture_output=True,
    )
    if description.returncode == 0:
        tag, distance, _revision = description.stdout.strip().rsplit('-', 2)
        if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?', tag):
            raise ValueError(f'Invalid release tag: {tag}')
        return tag if distance == '0' else f'{tag}-dev.{distance}'
    count = subprocess.check_output(['git', 'rev-list', '--count', 'HEAD'], cwd=cwd, text=True).strip()
    return f'dev.{count}'


if __name__ == '__main__':
    print(build_version())
