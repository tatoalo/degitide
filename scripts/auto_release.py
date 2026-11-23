#!/usr/bin/env python3
"""Publish a patch release for the current main commit after successful push CI."""
import json
import os
import re
import subprocess
import time


STABLE_TAG = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)")
WORKFLOW = "publish-images.yml"


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def api(path, **fields):
    args = ["gh", "api", f"repos/{os.environ['GH_REPO']}/{path}"]
    if fields:
        args += ["--method", "POST"]
        for key, value in fields.items():
            args += ["-f", f"{key}={value}"]
    try:
        result = command(*args)
    except subprocess.CalledProcessError as error:
        if not fields and "(HTTP 404)" in error.stderr:
            return None
        raise
    return json.loads(result) if result else None


def latest_stable_tag(tags):
    versions = [(tuple(map(int, match.groups())), tag) for tag in tags
                if (match := STABLE_TAG.fullmatch(tag))]
    if not versions:
        raise RuntimeError("Create an initial stable vX.Y.Z tag before enabling automatic releases.")
    return max(versions)[1]


def next_patch(tag):
    major, minor, patch = map(int, STABLE_TAG.fullmatch(tag).groups())
    return f"v{major}.{minor}.{patch + 1}"


def published(tag):
    release = api(f"releases/tags/{tag}")
    return bool(release and not release["draft"] and not release["prerelease"])


def ensure_published(tag, sha, timeout=75 * 60):
    """Resume an existing publisher, or dispatch once if tag creation was interrupted."""
    deadline = time.monotonic() + timeout
    dispatched = False
    run_seen = False
    while time.monotonic() < deadline:
        if published(tag):
            print(f"{tag} is published.", flush=True)
            return
        runs = api(f"actions/workflows/{WORKFLOW}/runs?head_sha={sha}&per_page=100")
        matching = [run for run in runs["workflow_runs"]
                    if run["head_branch"] == tag and run["head_sha"] == sha
                    and run["event"] in ("push", "workflow_dispatch")]
        if matching:
            run = max(matching, key=lambda item: item["id"])
            if not run_seen:
                print(f"Waiting for {run['html_url']}", flush=True)
                run_seen = True
            if run["status"] == "completed":
                # Publication and the run listing may become visible a moment apart.
                if published(tag):
                    return
                raise RuntimeError(
                    f"{tag} publisher finished with {run['conclusion']} without a release. "
                    f"Fix or rerun {run['html_url']}, then rerun Automatic release."
                )
        elif not dispatched:
            # GITHUB_TOKEN-created tags do not trigger push workflows. Dispatch is explicit.
            command("gh", "workflow", "run", WORKFLOW, "--ref", tag)
            dispatched = True
            print(f"Dispatched {WORKFLOW} for {tag}.", flush=True)
        time.sleep(20)
    raise RuntimeError(f"Timed out waiting for {tag}; rerun Automatic release to resume.")


def is_current_main(sha):
    return api("git/ref/heads/main")["object"]["sha"] == sha


def release(sha):
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or command("git", "rev-parse", "HEAD") != sha:
        raise RuntimeError("Checkout must match the successful CI commit.")
    if not is_current_main(sha):
        print("Skipping an older main commit; newer CI will release the current head.")
        return
    message = command("git", "log", "-1", "--format=%B")
    if "[skip release]" in message.lower():
        print("Skipping release as requested by [skip release].")
        return

    tag = latest_stable_tag(command("git", "tag", "--list").splitlines())
    previous_sha = command("git", "rev-parse", f"{tag}^{{commit}}")
    command("git", "merge-base", "--is-ancestor", previous_sha, sha)
    # Finish any previous release before reserving a new version or promoting latest.
    ensure_published(tag, previous_sha)
    if previous_sha == sha:
        return
    if not is_current_main(sha):
        print("Main advanced while waiting for publication; skipping this commit.")
        return

    tag = next_patch(tag)
    api("git/refs", ref=f"refs/tags/{tag}", sha=sha)
    print(f"Created {tag} at {sha}.", flush=True)
    ensure_published(tag, sha)


if __name__ == "__main__":
    try:
        release(os.environ["RELEASE_SHA"])
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.stderr or str(error)) from error
