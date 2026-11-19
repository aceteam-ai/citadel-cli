#!/usr/bin/env python3
"""Prepare a version-only edit or inspect immutable release candidates.

This does not authorize, build, sign, tag, upload or publish a release. The CI
caller must separately verify a reviewed merged bump PR and exact-head gates.
"""

import argparse
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import tempfile

MAX_VERSION_BYTES = 32
VERSION_PATTERN = re.compile(
    rb'(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\n'
)
COMMIT_PATTERN = re.compile(r'[0-9a-f]{40}')


class VersionError(Exception):
    """A refused version operation, with a fixed non-sensitive explanation."""


def parse_version(data):
    if not 0 < len(data) <= MAX_VERSION_BYTES:
        raise VersionError('VERSION exceeds its size bound or is empty')
    match = VERSION_PATTERN.fullmatch(data)
    if match is None:
        raise VersionError(
            'VERSION must contain one canonical stable version and a final LF'
        )
    return tuple(int(part) for part in match.groups())


def _git(repo, *args, check=True):
    result = subprocess.run(
        ['git', '-C', str(repo), *args], capture_output=True, timeout=10
    )
    if check and result.returncode:
        raise VersionError('Git snapshot operation failed')
    return result


def _snapshot(repo, revision):
    entry = _git(repo, 'ls-tree', '-z', revision, '--', 'VERSION').stdout
    if not entry:
        return None
    fields = entry.split(b'\t', 1)[0].split()
    if len(fields) != 3 or fields[:2] != [b'100644', b'blob']:
        raise VersionError('VERSION snapshot must be a non-executable regular file')
    object_id = fields[2].decode('ascii')
    size = int(_git(repo, 'cat-file', '-s', object_id).stdout)
    if not 0 < size <= MAX_VERSION_BYTES:
        raise VersionError('VERSION snapshot exceeds its size bound or is empty')
    data = _git(repo, 'cat-file', 'blob', object_id).stdout
    parse_version(data)
    return data


def plan(repo, before, after):
    for revision in (before, after):
        if not COMMIT_PATTERN.fullmatch(revision) or revision == '0' * 40:
            raise VersionError(
                'event revisions must be complete nonzero Git commit IDs'
            )
    for revision in (before, after):
        if _git(repo, 'cat-file', '-t', revision).stdout.strip() != b'commit':
            raise VersionError('event revisions must name commit objects')
    ancestry = _git(repo, 'merge-base', '--is-ancestor', before, after, check=False)
    if ancestry.returncode == 1:
        raise VersionError('before must be an ancestor of after')
    if ancestry.returncode:
        raise VersionError('Git ancestry check failed')
    previous, current = _snapshot(repo, before), _snapshot(repo, after)
    if current is None:
        if previous is not None:
            raise VersionError('VERSION deletion is not a release candidate')
        return {'candidate': False, 'reason': 'not-configured'}
    if previous is None:
        return {'candidate': False, 'reason': 'baseline-created'}
    if current == previous:
        return {'candidate': False, 'reason': 'unchanged'}
    if parse_version(current) <= parse_version(previous):
        raise VersionError('VERSION must increase')
    version = current.decode('ascii').strip()
    tag = 'v' + version
    existing = _git(
        repo, 'show-ref', '--verify', '--quiet', f'refs/tags/{tag}', check=False
    )
    if existing.returncode == 0:
        peeled = _git(repo, 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}')
        if peeled.stdout.decode('ascii').strip() != after:
            raise VersionError('release tag already names a different commit')
    elif existing.returncode != 1:
        raise VersionError('Git release-tag check failed')
    return {
        'candidate': True,
        'reason': 'version-bump',
        'version': version,
        'tag': tag,
        'commit': after,
    }


def _stamp(metadata):
    # Reading may update atime; that is not an edit or replacement.
    return (
        metadata.st_dev,
        metadata.st_ino,
        metadata.st_size,
        metadata.st_mode,
        metadata.st_mtime_ns,
        metadata.st_ctime_ns,
        metadata.st_uid,
        metadata.st_gid,
    )


def bump(repo, target):
    branch = _git(repo, 'symbolic-ref', '--quiet', '--short', 'HEAD', check=False)
    if branch.returncode or branch.stdout.strip() in (b'main', b'production'):
        raise VersionError('create a feature branch before bumping VERSION')
    path = pathlib.Path(repo) / 'VERSION'
    metadata = path.lstat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o111:
        raise VersionError('VERSION must be a non-executable regular file')
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_NONBLOCK', 0)
    with os.fdopen(os.open(path, flags), 'rb') as source:
        if _stamp(os.fstat(source.fileno())) != _stamp(metadata):
            raise VersionError(
                'VERSION changed before the read; retry after inspection'
            )
        current = source.read(MAX_VERSION_BYTES + 1)
    parts = list(parse_version(current))
    head = _git(repo, 'rev-parse', 'HEAD').stdout.decode('ascii').strip()
    if _snapshot(repo, head) != current:
        raise VersionError(
            'VERSION has uncommitted changes; commit or restore them first'
        )
    if target in ('major', 'minor', 'patch'):
        index = ('major', 'minor', 'patch').index(target)
        parts[index] += 1
        parts[index + 1 :] = [0] * (2 - index)
        target = '.'.join(str(part) for part in parts)
    if len(target) >= MAX_VERSION_BYTES:
        raise VersionError('target version exceeds its size bound')
    try:
        wanted = (target + '\n').encode('ascii')
    except UnicodeError:
        raise VersionError('target version must use ASCII digits') from None
    if parse_version(wanted) <= parse_version(current):
        raise VersionError('VERSION must increase')
    # The only mutation is this local file. No commit, branch, tag or remote write.
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(
            dir=repo, prefix='.VERSION.', delete=False
        ) as output:
            temporary = pathlib.Path(output.name)
            output.write(wanted)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, stat.S_IMODE(metadata.st_mode))
        if _stamp(path.lstat()) != _stamp(metadata):
            raise VersionError(
                'VERSION changed during the bump; retry after inspection'
            )
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return target


def main(argv=None, repo=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    bump_parser = commands.add_parser(
        'bump', help='edit VERSION locally on a feature branch'
    )
    bump_parser.add_argument(
        'target', help='major, minor, patch or an explicit stable version'
    )
    plan_parser = commands.add_parser(
        'plan', help='read a candidate; not release authorization'
    )
    plan_parser.add_argument('--before', required=True)
    plan_parser.add_argument('--after', required=True)
    args = parser.parse_args(argv)
    repo = (
        pathlib.Path(repo)
        if repo is not None
        else pathlib.Path(__file__).resolve().parent.parent
    )
    try:
        if args.command == 'bump':
            print(
                json.dumps(
                    {'version': bump(repo, args.target), 'release_authorized': False}
                )
            )
        else:
            print(json.dumps(plan(repo, args.before, args.after), sort_keys=True))
    except VersionError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, UnicodeError, ValueError, subprocess.SubprocessError):
        print('Could not complete the version operation', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
