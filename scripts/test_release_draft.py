"""Offline release orchestration proofs. All forge/build/sign transports are fake."""

import contextlib
import copy
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'release_draft', pathlib.Path(__file__).with_name('release_draft.py')
)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

SHA = 'a' * 40
TAG = 'v2.242.0'


def run_row(path, event):
    return {
        'id': 12,
        'path': path,
        'event': event,
        'head_branch': 'main',
        'head_sha': SHA,
        'status': 'completed',
        'conclusion': 'success',
        'repository': {'full_name': release.REPO},
        'head_repository': {'full_name': release.REPO},
        'check_suite_id': 4,
    }


def pr_row():
    return {
        'number': 5,
        'merged_at': '2026-11-19T00:00:00Z',
        'merge_commit_sha': SHA,
        'base': {'ref': 'main', 'repo': {'full_name': release.REPO}},
        'head': {'repo': {'full_name': release.REPO}},
    }


def check_row():
    return {
        'id': 1,
        'name': 'Build + Test',
        'status': 'completed',
        'conclusion': 'success',
        'app': {'slug': 'github-actions'},
        'check_suite': {'id': 3},
    }


class Fake(release.Runner):
    """No actual commands, credentials, sockets or key reads."""

    def __init__(self, root):
        super().__init__(root)
        self.calls = []
        self.packaged = {}
        self.dirty = False
        self.main = SHA
        self.changed = False
        self.receipt = {
            'tag': TAG,
            'commit': SHA,
            'run_id': 12,
            'ci_run': 11,
            'pr': 5,
            'checksums_sha256': '',
        }
        self.bytes = {
            name: ('synthetic ' + name).encode() for name in release.archive_names(TAG)
        }
        self.bytes['checksums.txt'] = ''.join(
            f'{hashlib.sha256(data).hexdigest()}  {name}\n'
            for name, data in self.bytes.items()
        ).encode()
        self.receipt['checksums_sha256'] = hashlib.sha256(
            self.bytes['checksums.txt']
        ).hexdigest()
        self.logs = release.MARKER + json.dumps(self.receipt)
        self.release = {
            'id': 8,
            'tag_name': TAG,
            'target_commitish': SHA,
            'draft': True,
            'prerelease': False,
            'body': self.logs,
        }
        self.sync_assets()
        self.runs = {
            '11': run_row('.github/workflows/ci.yml', 'push'),
            '12': run_row(release.WORKFLOW, 'workflow_run'),
        }
        self.checks = [check_row()]
        self.pr = pr_row()
        self.listed = True
        self.fault = None
        self.release_reads = 0

    def sync_assets(self):
        self.release['assets'] = [
            {
                'id': i + 20,
                'name': name,
                'size': len(data),
                'state': 'uploaded',
                'digest': 'sha256:' + hashlib.sha256(data).hexdigest(),
                'updated_at': 'fixed',
            }
            for i, (name, data) in enumerate(self.bytes.items())
        ]

    def api(self, path):
        self.calls.append(('api', path))
        if path.startswith('actions/runs/'):
            return copy.deepcopy(self.runs[path.rsplit('/', 1)[1]])
        if path == 'pulls/5':
            return copy.deepcopy(self.pr)
        if path == 'git/ref/tags/' + TAG:
            return {'object': {'type': 'commit', 'sha': SHA}}
        if path == 'releases/8':
            self.release_reads += 1
            result = copy.deepcopy(self.release)
            if self.changed and self.release_reads == 2:
                result['assets'][0]['id'] += 100
            return result
        raise AssertionError(path)

    def pages(self, path, key=None):
        self.calls.append(('pages', path))
        if 'check-runs' in path:
            return copy.deepcopy(self.checks)
        if '/pulls?' in path:
            return [copy.deepcopy(self.pr)]
        if path == 'releases':
            return [copy.deepcopy(self.release)] if self.listed else []
        raise AssertionError(path)

    def git(self, *args):
        self.calls.append(('git', args))
        if args[0] == 'status':
            return '?? stray.go' if self.dirty else ''
        if args[:2] == ('rev-parse', 'origin/main'):
            return self.main
        if args[0] == 'rev-parse':
            return SHA
        if args[0] == 'ls-remote':
            return ''
        return ''

    def download(self, asset_id, path, size):
        self.calls.append(('download', path.name))
        path.write_bytes(self.bytes[path.name])
        if self.fault == 'tampered-archive' and path.name.endswith(
            'linux_amd64.tar.gz'
        ):
            path.write_bytes(b'changed after CI')
        if self.fault == 'tampered-manifest' and path.name == 'checksums.txt':
            path.write_bytes(b'replaced manifest')

    def run(self, argv, *, env=None, timeout=60):
        self.calls.append(('run', argv))
        if self.fault == 'upload' and argv[:3] == ['gh', 'release', 'upload']:
            raise release.Refused('synthetic uncertain upload')
        if argv[:2] == ['go', 'build']:
            assert env['GOMAXPROCS'] == '4' and env['GOFLAGS'] == '-p=4'
            assert env['CGO_ENABLED'] == '0'
            pathlib.Path(argv[argv.index('-o') + 1]).write_bytes(b'synthetic binary')
        elif argv[:4] == ['go', 'run', './scripts/releasesign', 'verify-assets']:
            directory = pathlib.Path(argv[argv.index('--dir') + 1])
            for line in (directory / 'checksums.txt').read_text().splitlines():
                digest, name = line.split()
                release.require(
                    hashlib.sha256((directory / name).read_bytes()).hexdigest()
                    == digest,
                    'synthetic archive digest mismatch',
                )
        elif argv[:4] == ['go', 'run', './scripts/releasesign', 'sign']:
            pathlib.Path(argv[argv.index('--out') + 1]).write_bytes(
                b'synthetic signature'
            )
        elif argv[:3] == ['gh', 'run', 'view']:
            return self.logs
        elif argv[:3] == ['gh', 'release', 'upload']:
            self.bytes['checksums.txt.sig'] = pathlib.Path(argv[4]).read_bytes()
            self.sync_assets()
        elif argv[:3] == ['gh', 'release', 'create']:
            for item in argv:
                if item.endswith('.zip'):
                    with release.zipfile.ZipFile(item) as archive:
                        self.packaged[pathlib.Path(item).name] = archive.namelist()
                elif item.endswith('.tar.gz'):
                    with release.tarfile.open(item) as archive:
                        self.packaged[pathlib.Path(item).name] = archive.getnames()
        elif argv[:3] == ['gh', 'api', '--method']:
            self.release['draft'] = False
        return ''

    def has_command(self, prefix):
        return any(
            kind == 'run' and args[: len(prefix)] == prefix for kind, args in self.calls
        )


class ReleaseDraftTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='draft-proof-')
        self.addCleanup(self.tmp.cleanup)
        self.fake = Fake(pathlib.Path(self.tmp.name))
        protocol = pathlib.Path(self.tmp.name) / 'internal/protocol'
        protocol.mkdir(parents=True)
        (protocol / 'protocol.go').write_text('const FabricProtocolVersion = 1\n')
        self.env = mock.patch.dict(
            os.environ,
            {'CITADEL_RELEASE_SIGNING_KEY': '/synthetic/key.pem'},
            clear=True,
        )
        self.env.start()
        self.addCleanup(self.env.stop)

    def sign(self):
        with contextlib.redirect_stdout(io.StringIO()):
            release.sign_draft(self.fake, TAG)

    def test_platform_contract_is_exact(self):
        names = release.archive_names('v2.242.0')
        self.assertEqual(len(names), 6)
        self.assertIn('citadel_v2.242.0_windows_arm64.zip', names)
        self.assertIn('citadel_v2.242.0_linux_amd64.tar.gz', names)

    def test_sign_upload_publish_and_tap_are_ordered(self):
        self.sign()
        commands = [args for kind, args in self.fake.calls if kind == 'run']
        steps = [
            command[3]
            if command[:3] == ['go', 'run', './scripts/releasesign']
            else command[2]
            if command[:2] == ['gh', 'release']
            else command[0]
            for command in commands
        ]
        self.assertLess(steps.index('verify-assets'), steps.index('sign'))
        self.assertLess(steps.index('sign'), steps.index('verify'))
        self.assertLess(steps.index('verify'), steps.index('upload'))
        self.assertLess(steps.index('upload'), steps.index('bash'))
        self.assertFalse(self.fake.release['draft'])
        publish = next(
            command for command in commands if command[:3] == ['gh', 'api', '--method']
        )
        self.assertIn('repos/aceteam-ai/citadel-cli/releases/8', publish)
        self.assertFalse(any('--clobber' in command for command in commands))

    def test_archive_and_manifest_tampering_stop_before_key_use(self):
        for fault in ('tampered-archive', 'tampered-manifest'):
            with self.subTest(fault=fault):
                self.fake = Fake(pathlib.Path(self.tmp.name))
                self.fake.fault = fault
                with self.assertRaises(release.Refused):
                    self.sign()
                self.assertFalse(
                    self.fake.has_command(
                        ['go', 'run', './scripts/releasesign', 'sign']
                    )
                )
                self.assertTrue(self.fake.release['draft'])

    def test_matching_forged_archives_and_manifest_cannot_replace_ci_receipt(self):
        self.fake.bytes[next(iter(self.fake.bytes))] = b'forged archive'
        self.fake.bytes['checksums.txt'] = ''.join(
            f'{hashlib.sha256(self.fake.bytes[name]).hexdigest()}  {name}\n'
            for name in release.archive_names(TAG)
        ).encode()
        self.fake.sync_assets()
        with self.assertRaisesRegex(release.Refused, 'successful CI build'):
            self.sign()
        self.assertFalse(
            self.fake.has_command(['go', 'run', './scripts/releasesign', 'sign'])
        )

    def test_wrong_ci_identity_or_edited_receipt_stops_before_download(self):
        for field, value in [
            ('head_sha', 'b' * 40),
            ('event', 'pull_request'),
            ('path', '.github/workflows/other.yml'),
            ('conclusion', 'failure'),
            ('head_repository', {'full_name': 'foreign/repo'}),
        ]:
            with self.subTest(field=field):
                self.fake = Fake(pathlib.Path(self.tmp.name))
                self.fake.runs['12'][field] = value
                with self.assertRaises(release.Refused):
                    self.sign()
                self.assertFalse(any(k == 'download' for k, _ in self.fake.calls))
        self.fake = Fake(pathlib.Path(self.tmp.name))
        self.fake.logs = self.fake.logs.replace('a' * 40, 'b' * 40)
        with self.assertRaisesRegex(release.Refused, 'CI log receipt'):
            self.sign()

    def test_changed_draft_and_uncertain_upload_never_publish(self):
        for kind in ('changed', 'upload'):
            self.fake = Fake(pathlib.Path(self.tmp.name))
            self.fake.changed = kind == 'changed'
            self.fake.fault = 'upload' if kind == 'upload' else None
            with self.assertRaises(release.Refused):
                self.sign()
            self.assertFalse(self.fake.has_command(['gh', 'api', '--method']))
            self.assertFalse(self.fake.has_command(['bash']))

    def test_published_signed_retry_only_verifies_and_finishes_tap(self):
        self.fake.bytes['checksums.txt.sig'] = b'synthetic signature'
        self.fake.sync_assets()
        self.fake.release['draft'] = False
        self.sign()
        self.assertFalse(
            self.fake.has_command(['go', 'run', './scripts/releasesign', 'sign'])
        )
        self.assertFalse(self.fake.has_command(['gh', 'release', 'upload']))
        self.assertFalse(self.fake.has_command(['gh', 'api', '--method']))
        self.assertTrue(
            self.fake.has_command(['bash', 'scripts/update-homebrew-tap.sh'])
        )

    def test_branch_valued_commitish_still_requires_the_exact_remote_tag(self):
        self.fake.release['target_commitish'] = 'main'
        self.sign()
        self.fake = Fake(pathlib.Path(self.tmp.name))
        original = self.fake.api

        def wrong_tag(path):
            if path == 'git/ref/tags/' + TAG:
                return {'object': {'type': 'commit', 'sha': 'b' * 40}}
            return original(path)

        with mock.patch.object(self.fake, 'api', side_effect=wrong_tag):
            with self.assertRaisesRegex(release.Refused, 'remote release tag'):
                self.sign()
        self.assertFalse(
            self.fake.has_command(['go', 'run', './scripts/releasesign', 'sign'])
        )

    def test_ci_cannot_sign_and_dirty_or_stale_checkout_refused(self):
        os.environ['GITHUB_ACTIONS'] = 'true'
        with self.assertRaisesRegex(release.Refused, 'forbidden in CI'):
            self.sign()
        del os.environ['GITHUB_ACTIONS']
        for dirty, main in ((True, SHA), (False, 'b' * 40)):
            self.fake.dirty, self.fake.main = dirty, main
            with self.assertRaises(release.Refused):
                self.sign()
            self.assertFalse(self.fake.has_command(['go']))

    def test_red_pending_and_latest_attempt_gates(self):
        for conclusion, status in [
            ('failure', 'completed'),
            ('success', 'in_progress'),
            ('skipped', 'completed'),
        ]:
            check = check_row()
            check.update(conclusion=conclusion, status=status)
            with self.assertRaises(release.Refused):
                release.green_checks([check])
        older = check_row()
        older.update(conclusion='failure', id=0)
        release.green_checks([older, check_row()])
        pending = check_row()
        pending.update(id=2, status='in_progress', conclusion=None)
        with self.assertRaises(release.Refused):
            release.green_checks([check_row(), pending])

    def test_ci_no_bump_has_no_build_or_remote_write(self):
        with (
            mock.patch.object(
                release.version, 'plan', return_value={'candidate': False}
            ),
            contextlib.redirect_stdout(io.StringIO()),
        ):
            release.ci_draft(self.fake, SHA, '11', '12')
        self.assertFalse(self.fake.has_command(['go']))
        self.assertFalse(any(k == 'git' and v[0] == 'push' for k, v in self.fake.calls))

    def test_ci_builds_six_unsigned_archives_and_only_creates_draft(self):
        self.fake.listed = False
        with (
            mock.patch.object(
                release.version, 'plan', return_value={'candidate': True, 'tag': TAG}
            ),
            contextlib.redirect_stdout(io.StringIO()) as output,
        ):
            release.ci_draft(self.fake, SHA, '11', '12')
        receipt = release.receipt_from(output.getvalue())
        self.assertEqual(receipt['commit'], SHA)
        self.assertTrue(self.fake.has_command(['gh', 'release', 'create']))
        create = next(
            v
            for k, v in self.fake.calls
            if k == 'run' and v[:3] == ['gh', 'release', 'create']
        )
        self.assertIn('--draft', create)
        self.assertIn('--verify-tag', create)
        self.assertEqual(len([x for x in create if x.endswith(('.zip', '.tar.gz'))]), 6)
        self.assertFalse(
            self.fake.has_command(['go', 'run', './scripts/releasesign', 'sign'])
        )
        self.assertFalse(self.fake.has_command(['bash']))

        for name in release.archive_names(TAG):
            expected = (
                ['citadel.exe', 'citadel-start.exe']
                if name.endswith('.zip')
                else ['citadel']
            )
            self.assertEqual(self.fake.packaged[name], expected)

    def test_ci_refuses_existing_release_red_checks_or_main_movement(self):
        candidate = {'candidate': True, 'tag': TAG}
        with mock.patch.object(release.version, 'plan', return_value=candidate):
            with self.assertRaisesRegex(release.Refused, 'already exists'):
                release.ci_draft(self.fake, SHA, '11', '12')
        self.assertFalse(self.fake.has_command(['go', 'build']))
        self.fake = Fake(pathlib.Path(self.tmp.name))
        self.fake.listed = False
        self.fake.checks[0]['conclusion'] = 'failure'
        with mock.patch.object(release.version, 'plan', return_value=candidate):
            with self.assertRaises(release.Refused):
                release.ci_draft(self.fake, SHA, '11', '12')
        self.assertFalse(self.fake.has_command(['go', 'build']))
        self.fake = Fake(pathlib.Path(self.tmp.name))
        self.fake.listed = False
        with (
            mock.patch.object(release.version, 'plan', return_value=candidate),
            mock.patch.object(release, 'clean_main', side_effect=[SHA, 'b' * 40]),
        ):
            with self.assertRaisesRegex(release.Refused, 'main moved'):
                release.ci_draft(self.fake, SHA, '11', '12')
        self.assertFalse(any(k == 'git' and v[0] == 'push' for k, v in self.fake.calls))

    def test_foreign_unmerged_or_wrong_snapshot_pr_refused(self):
        for mutate in (
            lambda p: p.update(merged_at=None),
            lambda p: p.update(merge_commit_sha='b' * 40),
            lambda p: p['head']['repo'].update(full_name='foreign/repo'),
        ):
            pr = pr_row()
            mutate(pr)
            with self.assertRaises(release.Refused):
                release.bumped_pr([pr], SHA)

    def test_asset_shape_bounds_and_receipt_ambiguity(self):
        for change in (
            lambda r: r['assets'].append(copy.deepcopy(r['assets'][0])),
            lambda r: r['assets'][0].update(size=release.MAX_ARCHIVE + 1),
            lambda r: r['assets'][0].update(name='../escape'),
            lambda r: r.update(prerelease=True),
        ):
            value = copy.deepcopy(self.fake.release)
            change(value)
            with self.assertRaises(release.Refused):
                release.asset_snapshot(value, TAG)
        with self.assertRaises(release.Refused):
            release.receipt_from(self.fake.logs + '\n' + self.fake.logs)

    def test_real_download_adapter_refuses_overlong_truncated_and_failed_streams(self):
        for data, status, accepted in [
            (b'ab', 0, True),
            (b'abc', 0, False),
            (b'a', 0, False),
            (b'ab', 1, False),
        ]:
            with self.subTest(data=data, status=status):
                path = pathlib.Path(self.tmp.name) / 'download'
                path.unlink(missing_ok=True)
                process = SimpleNamespace(
                    stdout=io.BytesIO(data),
                    kill=mock.Mock(),
                    poll=lambda: status,
                    wait=mock.Mock(return_value=status),
                )
                with mock.patch.object(
                    release.subprocess, 'Popen', return_value=process
                ) as popen:
                    if accepted:
                        release.Runner(self.tmp.name).download(20, path, 2)
                        self.assertEqual(path.read_bytes(), data)
                    else:
                        with self.assertRaises(release.Refused):
                            release.Runner(self.tmp.name).download(20, path, 2)
                    self.assertIn('/releases/assets/20', popen.call_args.args[0][2])
                    self.assertNotIn('--pattern', popen.call_args.args[0])

    def test_pagination_bound_refuses_incomplete_gate_inventory(self):
        runner = release.Runner(self.tmp.name)
        with mock.patch.object(runner, 'api', return_value=[{}] * 100) as api:
            with self.assertRaisesRegex(release.Refused, 'pagination bound'):
                runner.pages('releases')
            self.assertEqual(api.call_count, 20)

    def test_command_errors_do_not_reveal_raw_output_or_arguments(self):
        error = release.subprocess.CalledProcessError(
            1, ['synthetic-private-argument'], stderr=b'synthetic-secret-value'
        )
        with mock.patch.object(release.subprocess, 'run', side_effect=error):
            with self.assertRaises(release.Refused) as result:
                release.Runner(self.tmp.name).run(['synthetic-private-argument'])
        self.assertNotIn('synthetic-', str(result.exception))

    def test_workflow_boundary_and_cpu_cap_are_pinned(self):
        root = pathlib.Path(__file__).resolve().parent.parent
        workflow = (root / '.github/workflows/release-draft.yml').read_text()
        for required in (
            'workflow_run:',
            'workflows: [CI]',
            'types: [completed]',
            'branches: [main]',
            "conclusion == 'success'",
            "event == 'push'",
            'head_repository.full_name == github.repository',
            'cancel-in-progress: false',
            'options: --cpus 4',
            "GOMAXPROCS: '4'",
            "GOFLAGS: '-p=4'",
            'ref: ${{ github.event.workflow_run.head_sha }}',
        ):
            self.assertIn(required, workflow)
        self.assertNotIn('secrets.', workflow)
        self.assertNotIn('upload-artifact', workflow)
        self.assertNotIn('release.sh', workflow)
        self.assertIn(
            'test_release_*.py', (root / '.github/workflows/ci.yml').read_text()
        )
        entry = (root / 'scripts/release.sh').read_text()
        self.assertLess(
            entry.index('release_draft.py'),
            entry.index('source "$SCRIPT_DIR/release-engine.sh"'),
        )
        self.assertIn('"${1:-}" != "--legacy"', entry)


if __name__ == '__main__':
    unittest.main()
