#!/usr/bin/env python3
"""Bundle release notes from first-parent Git history, without runtime GitHub access."""
import argparse
import json
import re
import subprocess
from pathlib import Path

STABLE = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)")


def git(*args, cwd=None):
    return subprocess.check_output(["git", *args], cwd=cwd, text=True).strip()


def version_key(tag):
    return tuple(map(int, STABLE.fullmatch(tag).groups()))


def changes(after, through, cwd=None):
    history = git("log", "--first-parent", "--format=%s%x00%b%x00",
                  f"{after}..{through}" if after else through, cwd=cwd)
    records = history.split("\0")
    notes = []
    for subject, body in zip(records[::2], records[1::2]):
        subject = subject.strip()
        # Preserve the PR title when a repository uses merge commits instead of squash.
        if subject.startswith("Merge pull request #") and body.strip():
            subject = body.strip().splitlines()[0]
        subject = re.sub(r"\s*\[skip (?:ci|release)\]", "", subject, flags=re.I).strip()
        if subject:
            notes.append(subject)
    return notes


def build_changelog(version, cwd=None, limit=10):
    if git("rev-parse", "--is-shallow-repository", cwd=cwd) == "true":
        raise ValueError("Changelog generation requires full Git history and tags (fetch-depth: 0).")
    tags = [tag for tag in git("tag", "--merged", "HEAD", cwd=cwd).splitlines()
            if STABLE.fullmatch(tag) and tag != version]
    if STABLE.fullmatch(version):
        tags = [tag for tag in tags if version_key(tag) < version_key(version)]
    elif match := re.fullmatch(r"(v\d+\.\d+\.\d+)-(?!dev\.|pr\.).+", version):
        # Rebuilding an old prerelease must not show a later stable release,
        # including a stable tag pointing at exactly the same commit.
        tags = [tag for tag in tags if version_key(tag) < version_key(match[1])]
    tags.sort(key=version_key, reverse=True)
    refs = [(version, "HEAD"), *((tag, tag) for tag in tags)]
    releases = []
    for index, (label, ref) in enumerate(refs[:limit]):
        previous = refs[index + 1][1] if index + 1 < len(refs) else None
        notes = changes(previous, ref, cwd)
        if not notes:
            if index == 0 and not STABLE.fullmatch(version):
                continue
            notes = [f"No code changes since {previous}." if previous else "Initial release."]
        releases.append({"version": label, "changes": notes})
    return {"version": version, "releases": releases}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("--output", type=Path, default=Path("internal/server/static/changelog.json"))
    args = parser.parse_args()
    data = build_changelog(args.version)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    print(f"Bundled changelog for {args.version}: {len(data['releases'])} releases")


if __name__ == "__main__":
    main()
