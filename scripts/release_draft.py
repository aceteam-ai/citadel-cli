#!/usr/bin/env python3
"""CI drafts unsigned releases; the operator signs/publishes locally.

No command retries a remote write. On an uncertain create/upload/publish, inspect
the remote state before rerunning. Signing keys are never accepted by CI mode.
"""

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import subprocess
import sys
import tarfile
import tempfile
import threading
import zipfile

REPO = 'aceteam-ai/citadel-cli'
WORKFLOW = '.github/workflows/release-draft.yml'
MARKER = 'CITADEL_DRAFT_RECEIPT='
PLATFORMS = (
    'linux_amd64',
    'linux_arm64',
    'darwin_amd64',
    'darwin_arm64',
    'windows_amd64',
    'windows_arm64',
)
MAX_ARCHIVE = 512 << 20
spec = importlib.util.spec_from_file_location(
    'release_version', pathlib.Path(__file__).with_name('release_version.py')
)
version = importlib.util.module_from_spec(spec)
spec.loader.exec_module(version)


class Refused(Exception):
    """Fixed, non-sensitive operator error."""


def require(condition, message):
    if not condition:
        raise Refused(message)


def archive_names(tag):
    require(tag.startswith('v'), 'invalid release tag')
    version.parse_version((tag[1:] + '\n').encode('ascii'))
    return [
        f'citadel_{tag}_{p}' + ('.zip' if p.startswith('windows') else '.tar.gz')
        for p in PLATFORMS
    ]


class Runner:
    def __init__(self, root):
        self.root = pathlib.Path(root)

    def run(self, argv, *, env=None, timeout=60):
        # Fixed errors only: gh stderr can contain remote data, and signing
        # errors can contain local key paths. Never echo argv or raw stderr.
        try:
            with tempfile.TemporaryFile() as output:
                subprocess.run(
                    argv,
                    cwd=self.root,
                    env=env,
                    check=True,
                    stdout=output,
                    stderr=subprocess.DEVNULL,
                    timeout=timeout,
                )
                output.seek(0)
                data = output.read((16 << 20) + 1)
                require(len(data) <= 16 << 20, 'release command output exceeds bound')
                return data.decode()
        except (OSError, UnicodeError, subprocess.SubprocessError):
            raise Refused(
                'release command failed; inspect state before retrying'
            ) from None

    def api(self, path):
        return json.loads(self.run(['gh', 'api', f'repos/{REPO}/{path}']))

    def pages(self, path, key=None):
        # Bound pagination and refuse truncation rather than silently omit gates.
        result = []
        for page in range(1, 21):
            separator = '&' if '?' in path else '?'
            data = self.api(f'{path}{separator}page={page}&per_page=100')
            rows = data[key] if key else data
            require(isinstance(rows, list), 'invalid forge page')
            result.extend(rows)
            if len(rows) < 100:
                return result
        raise Refused('forge pagination bound exceeded')

    def git(self, *args):
        return self.run(['git', *args]).strip()

    def download(self, asset_id, path, size):
        # Download immutable asset IDs with an independent byte/time bound.
        argv = [
            'gh',
            'api',
            f'repos/{REPO}/releases/assets/{asset_id}',
            '-H',
            'Accept: application/octet-stream',
        ]
        process = subprocess.Popen(
            argv, cwd=self.root, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL
        )
        timer = threading.Timer(300, process.kill)
        timer.start()
        try:
            with path.open('xb') as output:
                total = 0
                while block := process.stdout.read(min(65536, size + 1 - total)):
                    total += len(block)
                    require(total <= size, 'asset exceeded its declared size')
                    output.write(block)
            require(
                process.wait(timeout=5) == 0 and total == size,
                'asset download failed or was truncated',
            )
        finally:
            timer.cancel()
            if process.poll() is None:
                process.kill()
            process.wait()
            process.stdout.close()


def clean_main(runner):
    require(
        not runner.git('status', '--porcelain', '--untracked-files=all'),
        'release requires a clean checkout, including untracked files',
    )
    runner.git(
        'fetch', '-q', 'origin', '+refs/heads/main:refs/remotes/origin/main', '--tags'
    )
    head = runner.git('rev-parse', 'HEAD')
    require(
        head == runner.git('rev-parse', 'origin/main'),
        'release checkout must equal fresh origin/main',
    )
    return head


def green_checks(checks, own_suite=None):
    latest = {}
    for check in checks:
        if (
            own_suite is not None
            and check.get('check_suite', {}).get('id') == own_suite
        ):
            continue
        require(
            check.get('app', {}).get('slug') == 'github-actions',
            'unrecognized check provider; inspect the release gate',
        )
        name = check['name']
        if name not in latest or check['id'] > latest[name]['id']:
            latest[name] = check
    require('Build + Test' in latest, 'required Build + Test gate is absent')
    require(
        latest['Build + Test']['conclusion'] == 'success',
        'required Build + Test gate did not pass',
    )
    require(
        all(
            c['status'] == 'completed'
            and c['conclusion'] in ('success', 'neutral', 'skipped')
            for c in latest.values()
        ),
        'exact-commit checks are not all green',
    )


def ci_identity(run, commit, workflow, event):
    require(
        run.get('repository', {}).get('full_name') == REPO
        and run.get('head_repository', {}).get('full_name') == REPO
        and run.get('head_sha') == commit
        and run.get('head_branch') == 'main'
        and run.get('event') == event
        and run.get('path') == workflow
        and run.get('status') == 'completed'
        and run.get('conclusion') == 'success',
        'release CI provenance does not match the approved source',
    )


def bumped_pr(prs, commit):
    matches = [
        p
        for p in prs
        if p.get('merged_at')
        and p.get('merge_commit_sha') == commit
        and p.get('base', {}).get('ref') == 'main'
        and p.get('base', {}).get('repo', {}).get('full_name') == REPO
        and p.get('head', {}).get('repo', {}).get('full_name') == REPO
    ]
    require(
        len(matches) == 1, 'release must be the exact merge of one same-repo bump PR'
    )
    return matches[0]['number']


def digest(path):
    require(
        path.is_file() and not path.is_symlink() and path.stat().st_size <= MAX_ARCHIVE,
        'release file is nonregular or exceeds the size bound',
    )
    with path.open('rb') as file:
        return hashlib.file_digest(file, 'sha256').hexdigest()


def build_archives(runner, tag, directory):
    names = archive_names(tag)
    # Same gentle Go limits as release.sh, plus the CI container's hard CPU cap.
    environment = dict(os.environ, GOMAXPROCS='4', GOFLAGS='-p=4', CGO_ENABLED='0')
    for platform, name in zip(PLATFORMS, names):
        system, arch = platform.split('_')
        with tempfile.TemporaryDirectory(prefix='citadel-build-') as temporary:
            temporary = pathlib.Path(temporary)
            members = (
                ['citadel.exe', 'citadel-start.exe']
                if system == 'windows'
                else ['citadel']
            )
            for member in members:
                package = (
                    './cmd/citadel-start'
                    if member.startswith('citadel-start')
                    else './cmd/citadel'
                )
                runner.run(
                    [
                        'go',
                        'build',
                        '-trimpath',
                        '-buildvcs=false',
                        '-ldflags',
                        f'-X github.com/aceteam-ai/citadel-cli/cmd.version={tag[1:]}',
                        '-o',
                        str(temporary / member),
                        package,
                    ],
                    env=dict(environment, GOOS=system, GOARCH=arch),
                    timeout=900,
                )
            destination = directory / name
            if system == 'windows':
                with zipfile.ZipFile(
                    destination, 'w', compression=zipfile.ZIP_DEFLATED
                ) as archive:
                    for member in members:
                        archive.write(temporary / member, member)
            else:
                with tarfile.open(destination, 'w:gz') as archive:
                    archive.add(temporary / members[0], arcname=members[0])
    manifest = ''.join(f'{digest(directory / name)}  {name}\n' for name in names)
    (directory / 'checksums.txt').write_text(manifest, encoding='ascii')
    runner.run(
        [
            'go',
            'run',
            './scripts/releasesign',
            'verify-assets',
            '--tag',
            tag,
            '--in',
            str(directory / 'checksums.txt'),
            '--dir',
            str(directory),
        ],
        env=environment,
        timeout=300,
    )


def ci_draft(runner, commit, ci_run, run_id):
    require(re.fullmatch('[0-9a-f]{40}', commit), 'invalid source commit')
    require(
        re.fullmatch('[1-9][0-9]*', ci_run) and re.fullmatch('[1-9][0-9]*', run_id),
        'invalid workflow run id',
    )
    require(clean_main(runner) == commit, 'CI source is not fresh main')
    before = runner.git('rev-parse', commit + '^1')
    candidate = version.plan(runner.root, before, commit)
    if not candidate['candidate']:
        print(json.dumps(candidate))
        return
    tag = candidate['tag']
    ci_identity(
        runner.api(f'actions/runs/{ci_run}'), commit, '.github/workflows/ci.yml', 'push'
    )
    own = runner.api(f'actions/runs/{run_id}')
    require(
        own.get('path') == WORKFLOW
        and own.get('event') == 'workflow_run'
        and own.get('head_sha') == commit
        and own.get('head_branch') == 'main'
        and own.get('repository', {}).get('full_name') == REPO
        and isinstance(own.get('check_suite_id'), int),
        'invalid drafting workflow identity',
    )
    green_checks(
        runner.pages(f'commits/{commit}/check-runs?filter=all', 'check_runs'),
        own['check_suite_id'],
    )
    pr = bumped_pr(runner.pages(f'commits/{commit}/pulls?state=closed'), commit)
    # Refuse existing releases, even partial drafts. Never overwrite uncertain work.
    releases = runner.pages('releases')
    require(
        not any(r['tag_name'] == tag for r in releases),
        'release already exists; reconcile it instead of overwriting',
    )
    with tempfile.TemporaryDirectory(prefix='citadel-release-') as temporary:
        directory = pathlib.Path(temporary)
        build_archives(runner, tag, directory)
        require(
            clean_main(runner) == commit, 'main moved during the build; draft refused'
        )
        green_checks(
            runner.pages(f'commits/{commit}/check-runs?filter=all', 'check_runs'),
            own['check_suite_id'],
        )
        # A tag may survive a failed draft creation. Retry only at the exact SHA.
        remote = runner.git('ls-remote', '--refs', 'origin', f'refs/tags/{tag}')
        if remote:
            require(
                remote.split()[0] == commit,
                'existing remote tag differs from the source',
            )
        else:
            runner.git('push', 'origin', f'{commit}:refs/tags/{tag}')
        receipt = {
            'tag': tag,
            'commit': commit,
            'run_id': int(run_id),
            'ci_run': int(ci_run),
            'pr': pr,
            'checksums_sha256': digest(directory / 'checksums.txt'),
        }
        notes = directory / 'notes.txt'
        protocol_source = (runner.root / 'internal/protocol/protocol.go').read_text()
        protocol = re.findall(
            r'^const FabricProtocolVersion = ([0-9]+)$', protocol_source, re.MULTILINE
        )
        require(len(protocol) == 1, 'fabric protocol version could not be established')
        notes.write_text(
            'Unsigned CI draft. Operator signing is required.\n'
            + f'\n**Fabric protocol: v{protocol[0]}** '
            '(node/control-plane wire contract).\n'
            + MARKER
            + json.dumps(receipt, sort_keys=True)
            + '\n'
        )
        runner.run(
            [
                'gh',
                'release',
                'create',
                tag,
                '--repo',
                REPO,
                '--draft',
                '--verify-tag',
                '--target',
                commit,
                '--title',
                tag,
                '--notes-file',
                str(notes),
                *[str(directory / name) for name in archive_names(tag)],
                str(directory / 'checksums.txt'),
            ],
            timeout=900,
        )
        # Immutable Actions logs, not the editable release body, bind these bytes.
        print(MARKER + json.dumps(receipt, sort_keys=True))


def receipt_from(text):
    rows = [line.split(MARKER, 1)[1] for line in text.splitlines() if MARKER in line]
    require(len(rows) == 1, 'expected one CI draft receipt')
    receipt = json.loads(rows[0])
    require(
        set(receipt) == {'tag', 'commit', 'run_id', 'ci_run', 'pr', 'checksums_sha256'}
        and re.fullmatch('[0-9a-f]{40}', receipt['commit'])
        and re.fullmatch('[0-9a-f]{64}', receipt['checksums_sha256'])
        and all(
            type(receipt[k]) is int and receipt[k] > 0
            for k in ('run_id', 'ci_run', 'pr')
        ),
        'invalid draft receipt',
    )
    archive_names(receipt['tag'])
    return receipt


def asset_snapshot(release, tag):
    require(
        release.get('tag_name') == tag
        and release.get('prerelease') is False
        and type(release.get('id')) is int,
        'release metadata mismatch',
    )
    names = archive_names(tag) + ['checksums.txt']
    assets = release['assets']
    require(
        len(assets) in (7, 8) and len({a['name'] for a in assets}) == len(assets),
        'unexpected or duplicate release assets',
    )
    require(
        set(a['name'] for a in assets)
        in (set(names), set(names + ['checksums.txt.sig'])),
        'unexpected release assets',
    )
    for asset in assets:
        bound = 65536 if asset['name'].startswith('checksums.txt') else MAX_ARCHIVE
        require(
            asset.get('state') == 'uploaded'
            and type(asset.get('size')) is int
            and 0 < asset['size'] <= bound
            and type(asset.get('id')) is int,
            'asset is incomplete or exceeds the size bound',
        )
    return {
        a['name']: (a['id'], a['size'], a.get('digest'), a.get('updated_at'))
        for a in assets
    }


def sign_draft(runner, tag):
    archive_names(tag)
    require(os.environ.get('GITHUB_ACTIONS') != 'true', 'signing is forbidden in CI')
    clean_main(runner)
    releases = [r for r in runner.pages('releases') if r['tag_name'] == tag]
    require(len(releases) == 1, 'expected one release for the tag')
    release = runner.api(f'releases/{releases[0]["id"]}')
    initial = asset_snapshot(release, tag)
    receipt = receipt_from(release['body'])
    # GitHub may retain a branch-valued target_commitish for an existing tag.
    # Bind source through the exact remote tag and immutable CI log instead.
    require(receipt['tag'] == tag, 'draft tag/source mismatch')
    source = receipt['commit']
    runner.git('merge-base', '--is-ancestor', source, 'HEAD')
    ci_identity(
        runner.api(f'actions/runs/{receipt["run_id"]}'),
        source,
        WORKFLOW,
        'workflow_run',
    )
    logs = runner.run(
        ['gh', 'run', 'view', str(receipt['run_id']), '--repo', REPO, '--log'],
        timeout=120,
    )
    require(
        receipt_from(logs) == receipt, 'editable draft differs from the CI log receipt'
    )
    ci_identity(
        runner.api(f'actions/runs/{receipt["ci_run"]}'),
        source,
        '.github/workflows/ci.yml',
        'push',
    )
    # The original draft workflow has now completed, so every check must pass.
    green_checks(runner.pages(f'commits/{source}/check-runs?filter=all', 'check_runs'))
    pr = runner.api(f'pulls/{receipt["pr"]}')
    require(bumped_pr([pr], source) == receipt['pr'], 'bump PR provenance changed')
    tag_data = runner.api(f'git/ref/tags/{tag}')['object']
    require(
        tag_data.get('type') == 'commit' and tag_data.get('sha') == source,
        'remote release tag differs from the CI commit',
    )
    with tempfile.TemporaryDirectory(prefix='citadel-sign-') as temporary:
        directory = pathlib.Path(temporary)
        for name in initial:
            runner.download(initial[name][0], directory / name, initial[name][1])
        checksums = directory / 'checksums.txt'
        require(
            digest(checksums) == receipt['checksums_sha256'],
            'checksums differ from the successful CI build',
        )
        runner.run(
            [
                'go',
                'run',
                './scripts/releasesign',
                'verify-assets',
                '--tag',
                tag,
                '--in',
                str(checksums),
                '--dir',
                str(directory),
            ],
            timeout=300,
        )
        signature = directory / 'checksums.txt.sig'
        if not signature.exists():
            require(release.get('draft') is True, 'unsigned published release refused')
            key = os.environ.get('CITADEL_RELEASE_SIGNING_KEY') or str(
                pathlib.Path.home()
                / '.config/citadel-release/release-signing-ed25519.pem'
            )
            runner.run(
                [
                    'go',
                    'run',
                    './scripts/releasesign',
                    'sign',
                    '--key',
                    key,
                    '--tag',
                    tag,
                    '--in',
                    str(checksums),
                    '--out',
                    str(signature),
                ],
                timeout=300,
            )
        runner.run(
            [
                'go',
                'run',
                './scripts/releasesign',
                'verify',
                '--tag',
                tag,
                '--in',
                str(checksums),
                '--sig',
                str(signature),
            ],
            timeout=300,
        )
        # Re-read after all validation and signing. Never publish changed assets.
        current = runner.api(f'releases/{release["id"]}')
        require(
            current['id'] == release['id']
            and current['body'] == release['body']
            and current['target_commitish'] == release['target_commitish']
            and current['draft'] == release['draft']
            and asset_snapshot(current, tag) == initial,
            'draft changed during signing',
        )
        if 'checksums.txt.sig' not in initial:
            runner.run(
                ['gh', 'release', 'upload', tag, str(signature), '--repo', REPO],
                timeout=120,
            )
            uploaded = runner.api(f'releases/{release["id"]}')
            snapshot = asset_snapshot(uploaded, tag)
            require(
                uploaded['draft'] is True
                and uploaded['body'] == release['body']
                and uploaded['target_commitish'] == release['target_commitish']
                and {k: v for k, v in snapshot.items() if k != 'checksums.txt.sig'}
                == initial
                and snapshot.get('checksums.txt.sig', (None, None, None))[2]
                == 'sha256:' + digest(signature),
                'signature upload or draft state is uncertain',
            )
        if release['draft']:
            runner.run(
                [
                    'gh',
                    'api',
                    '--method',
                    'PATCH',
                    f'repos/{REPO}/releases/{release["id"]}',
                    '-F',
                    'draft=false',
                    '-f',
                    'make_latest=true',
                ]
            )
        # Published, verified retries can finish a failed tap update without
        # republishing, replacing any asset, or resigning.
        runner.run(
            ['bash', 'scripts/update-homebrew-tap.sh', tag[1:]],
            env=dict(os.environ, CHECKSUMS_FILE=str(checksums)),
            timeout=300,
        )
    print('Signed release published; Homebrew tap updated')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    ci = commands.add_parser('ci')
    ci.add_argument('--commit', required=True)
    ci.add_argument('--ci-run', required=True)
    ci.add_argument('--run', required=True)
    sign = commands.add_parser('sign')
    sign.add_argument('tag')
    args = parser.parse_args()
    runner = Runner(pathlib.Path(__file__).resolve().parent.parent)
    try:
        if args.command == 'ci':
            ci_draft(runner, args.commit, args.ci_run, args.run)
        else:
            sign_draft(runner, args.tag)
    except (Refused, version.VersionError) as error:
        print(f'Release refused: {error}. No automatic retries.', file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError):
        print(
            'Release refused. Inspect CI/draft state before retrying. '
            'No automatic retries.',
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
