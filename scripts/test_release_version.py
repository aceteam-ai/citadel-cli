"""Offline tests using only owned temporary Git repositories."""

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'release_version', pathlib.Path(__file__).with_name('release_version.py')
)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseVersionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='release-version-')
        self.addCleanup(self.tmp.cleanup)
        self.repo = pathlib.Path(self.tmp.name)
        self.git('init', '-q', '-b', 'work')
        self.git('config', 'user.name', 'Synthetic release fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.old = self.commit('2.241.0\n')

    def git(self, *args):
        return subprocess.run(
            ['git', '-C', str(self.repo), *args],
            check=True,
            capture_output=True,
            text=True,
            timeout=10,
        ).stdout.strip()

    def commit(self, version):
        if version is None:
            (self.repo / 'VERSION').unlink(missing_ok=True)
        else:
            (self.repo / 'VERSION').write_text(version)
        self.git('add', '-A')
        self.git('commit', '-q', '--allow-empty', '-m', 'Synthetic fixture')
        return self.git('rev-parse', 'HEAD')

    def test_only_a_real_upward_snapshot_change_is_candidate(self):
        unchanged = self.commit('2.241.0\n')
        self.assertEqual(
            release.plan(self.repo, self.old, unchanged)['reason'], 'unchanged'
        )
        newer = self.commit('2.242.0\n')
        # A dirty worktree must never replace the immutable event snapshot.
        (self.repo / 'VERSION').write_text('99.0.0\n')
        self.assertEqual(
            release.plan(self.repo, self.old, newer),
            {
                'candidate': True,
                'reason': 'version-bump',
                'version': '2.242.0',
                'tag': 'v2.242.0',
                'commit': newer,
            },
        )

    def test_baseline_and_unconfigured_push_do_not_release(self):
        empty = self.commit(None)
        empty2 = self.commit(None)
        self.assertFalse(release.plan(self.repo, empty, empty2)['candidate'])
        baseline = self.commit('2.241.0\n')
        self.assertEqual(
            release.plan(self.repo, empty, baseline),
            {
                'candidate': False,
                'reason': 'baseline-created',
            },
        )

    def test_downgrade_deletion_and_noncanonical_versions_refused(self):
        for value in [
            '2.240.0\n',
            None,
            'v2.242.0\n',
            '02.242.0\n',
            '2.242.0-rc.1\n',
            '2.242.0',
            '2.242.0\r\n',
            '2.242.0\n\n',
            '2.242.0\nanything',
            '2.242.000\n',
            '9999999999.0.0\n',
        ]:
            with self.subTest(value=value):
                after = self.commit(value)
                with self.assertRaises(release.VersionError):
                    release.plan(self.repo, self.old, after)

    def test_unrelated_history_and_untrusted_revision_strings_refused(self):
        self.git('checkout', '-q', '--orphan', 'unrelated')
        other = self.commit('2.242.0\n')
        with self.assertRaisesRegex(release.VersionError, 'ancestor'):
            release.plan(self.repo, self.old, other)
        for revision in ['HEAD', '--help', 'a' * 39, 'a' * 41, '0' * 40, 'g' * 40]:
            with (
                self.subTest(revision=revision),
                mock.patch.object(release, '_git') as git,
            ):
                with self.assertRaises(release.VersionError):
                    release.plan(self.repo, revision, other)
                git.assert_not_called()

    def test_retry_tag_must_peel_to_the_exact_candidate_commit(self):
        after = self.commit('2.242.0\n')
        self.git('tag', 'v2.242.0', self.old)
        with self.assertRaisesRegex(release.VersionError, 'different commit'):
            release.plan(self.repo, self.old, after)
        self.git('tag', '-d', 'v2.242.0')
        self.git('tag', '-a', 'v2.242.0', '-m', 'Synthetic fixture', after)
        self.assertTrue(release.plan(self.repo, self.old, after)['candidate'])

    def test_snapshot_symlink_and_oversize_blob_refused_before_contents_read(self):
        (self.repo / 'VERSION').unlink()
        (self.repo / 'VERSION').symlink_to('unrelated')
        self.git('add', 'VERSION')
        self.git('commit', '-qm', 'Synthetic symlink')
        with self.assertRaises(release.VersionError):
            release.plan(self.repo, self.old, self.git('rev-parse', 'HEAD'))
        (self.repo / 'VERSION').unlink()
        after = self.commit('x' * (1024 * 1024))
        large_object = self.git('rev-parse', after + ':VERSION')
        with mock.patch.object(release, '_git', wraps=release._git) as git:
            with self.assertRaises(release.VersionError):
                release.plan(self.repo, self.old, after)
            self.assertFalse(
                any(
                    call.args[1:] == ('cat-file', 'blob', large_object)
                    for call in git.call_args_list
                )
            )

    def test_tag_blob_and_tag_object_event_revision_refused(self):
        after = self.commit('2.242.0\n')
        blob = self.git('rev-parse', after + ':VERSION')
        self.git('update-ref', 'refs/tags/v2.242.0', blob)
        with self.assertRaises(release.VersionError):
            release.plan(self.repo, self.old, after)
        self.git('tag', '-a', 'synthetic-annotated', '-m', 'Synthetic tag', self.old)
        tag_object = self.git('rev-parse', 'refs/tags/synthetic-annotated')
        with self.assertRaisesRegex(release.VersionError, 'commit objects'):
            release.plan(self.repo, tag_object, after)

    def test_bumper_changes_only_version_and_never_creates_a_commit_or_tag(self):
        (self.repo / 'keep.txt').write_text('synthetic unrelated work')
        self.assertEqual(release.bump(self.repo, 'minor'), '2.242.0')
        self.assertEqual((self.repo / 'VERSION').read_text(), '2.242.0\n')
        self.assertEqual(
            (self.repo / 'keep.txt').read_text(), 'synthetic unrelated work'
        )
        self.assertEqual(self.git('rev-parse', 'HEAD'), self.old)
        self.assertEqual(self.git('tag'), '')
        with self.assertRaisesRegex(release.VersionError, 'uncommitted'):
            release.bump(self.repo, 'patch')

    def test_bumper_refuses_main_downgrade_symlink_and_invalid_target(self):
        for branch in ['main', 'production']:
            self.git('checkout', '-qb', branch)
            with self.subTest(branch=branch), self.assertRaises(release.VersionError):
                release.bump(self.repo, 'minor')
        self.git('checkout', '-q', '--detach', self.old)
        with self.assertRaises(release.VersionError):
            release.bump(self.repo, 'patch')
        self.git('checkout', '-q', 'work')
        for target in [
            '2.240.0',
            '2.241.0',
            '../outside',
            '2.242.0; echo synthetic',
            '2.242.0-rc.1',
        ]:
            with self.subTest(target=target), self.assertRaises(release.VersionError):
                release.bump(self.repo, target)
        self.assertEqual((self.repo / 'VERSION').read_text(), '2.241.0\n')
        (self.repo / 'VERSION').unlink()
        (self.repo / 'VERSION').symlink_to('keep.txt')
        with self.assertRaises(release.VersionError):
            release.bump(self.repo, 'patch')

    def test_patch_major_and_explicit_bumps(self):
        for target, expected in [
            ('patch', '2.241.1'),
            ('major', '3.0.0'),
            ('3.1.2', '3.1.2'),
        ]:
            with self.subTest(target=target):
                (self.repo / 'VERSION').write_text('2.241.0\n')
                self.assertEqual(release.bump(self.repo, target), expected)

    def test_atime_is_not_a_concurrent_edit_and_late_real_edit_is_preserved(self):
        path = self.repo / 'VERSION'
        metadata = path.stat()
        os.utime(path, ns=(1, metadata.st_mtime_ns))
        self.assertEqual(release.bump(self.repo, 'patch'), '2.241.1')
        path.write_text('2.241.0\n')
        snapshot = release._snapshot

        def concurrent_edit(repo, revision):
            result = snapshot(repo, revision)
            path.write_text('2.241.9\n')
            return result

        with mock.patch.object(release, '_snapshot', side_effect=concurrent_edit):
            with self.assertRaisesRegex(release.VersionError, 'changed during'):
                release.bump(self.repo, 'minor')
        self.assertEqual(path.read_text(), '2.241.9\n')
        self.assertEqual(list(self.repo.glob('.VERSION.*')), [])

    def test_cli_outputs_candidate_json_and_refuses_bad_request(self):
        after = self.commit('2.242.0\n')
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(
                release.main(
                    ['plan', '--before', self.old, '--after', after], self.repo
                ),
                0,
            )
        self.assertTrue(json.loads(stdout.getvalue())['candidate'])
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(release.main(['bump', '2.200.0'], self.repo), 1)
        self.assertEqual(stdout.getvalue(), '')
        self.assertNotIn(str(self.repo), stderr.getvalue())


if __name__ == '__main__':
    unittest.main()
