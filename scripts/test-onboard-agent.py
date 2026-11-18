#!/usr/bin/env python3
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('onboard_agent', Path(__file__).with_name('onboard-agent.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class OnboardAgentTests(unittest.TestCase):
    def test_endpoint_security(self):
        for url in ('http://remote.test', 'https://user:pass@remote.test', 'https://remote.test/path', 'http://localhost'):
            with self.assertRaises(ValueError):
                module.api_origin(url)
        self.assertEqual(module.api_origin('https://mail.example.test/'), 'https://mail.example.test')
        self.assertEqual(module.api_origin('http://127.0.0.1:8787'), 'http://127.0.0.1:8787')

    def test_profile_contains_only_agent_access(self):
        config = dict(name='test', username='user@example.test', imap_host='imap.test', imap_port=993,
                      imap_tls=True, smtp_host='smtp.test', smtp_port=465, smtp_tls=True)
        response = io.BytesIO(json.dumps({'mailbox_name': 'test', 'alias_email': 'agent@example.test',
                                         'token': 'alias-secret', 'password': 'should-not-export'}).encode())
        with patch.object(module.urllib.request, 'build_opener') as factory:
            factory.return_value.open.return_value = response
            result = module.onboard('http://127.0.0.1:8787', 'master-secret', config, 'provider-secret', 'agent@example.test', 'test-agent')
            request = factory.return_value.open.call_args.args[0]
            self.assertTrue(request.full_url.endswith('/v1/admin/onboard'))
            self.assertEqual(json.loads(request.data)['alias_email'], 'agent@example.test')
        self.assertEqual(set(result), {'version', 'api_url', 'mailbox', 'alias', 'token'})
        self.assertEqual(result['token'], 'alias-secret')
        self.assertNotIn('provider-secret', json.dumps(result))
        self.assertNotIn('master-secret', json.dumps(result))

    def test_private_profile_and_no_overwrite_or_secret_output(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            key = root / 'master.key'
            key.write_text('master-secret'); key.chmod(0o600)
            config = root / 'mailbox.json'
            config.write_text(json.dumps(dict(name='test', username='user@example.test', imap_host='imap.test', imap_port=993,
                imap_tls=True, smtp_host='smtp.test', smtp_port=465, smtp_tls=True)))
            target = root / 'agent.json'
            args = ['onboard-agent.py', '--master-key-file', str(key), '--config', str(config), '--alias', 'agent@example.test', '--profile', str(target)]
            out = io.StringIO()
            profile = dict(version=1, api_url='http://127.0.0.1:8787', mailbox='test', alias='agent@example.test', token='alias-secret')
            with patch.object(module.sys, 'argv', args), patch.object(module.sys.stdin, 'isatty', return_value=True), \
                    patch.object(module.getpass, 'getpass', return_value='provider-secret'), \
                    patch.object(module, 'onboard', return_value=profile) as submit, contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
                self.assertEqual(module.main(), 0)
                self.assertEqual(target.stat().st_mode & 0o777, 0o600)
                self.assertEqual(json.loads(target.read_text()), profile)
                self.assertEqual(module.main(), 1)
                self.assertEqual(submit.call_count, 1)
            for secret in ('alias-secret', 'provider-secret', 'master-secret'):
                self.assertNotIn(secret, out.getvalue())

    def test_existing_agent_recovery_and_failed_retry_cleanup(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            key = root / 'master.key'; key.write_text('master-secret'); key.chmod(0o600)
            profile = root / 'agent.json'
            args = ['onboard-agent.py', '--master-key-file', str(key), '--existing-mailbox', 'test',
                    '--alias', 'agent@example.test', '--token-name', 'agent', '--replace', '--profile', str(profile)]
            out = io.StringIO()
            with patch.object(module.sys, 'argv', args), patch.object(module.getpass, 'getpass') as prompt, \
                    patch.object(module, 'onboard', side_effect=RuntimeError('provider rejected')) as submit, \
                    contextlib.redirect_stderr(out):
                self.assertEqual(module.main(), 1)
                self.assertFalse(profile.exists(), 'empty reservation must not block corrected retry')
                prompt.assert_not_called()
                self.assertEqual(submit.call_args.kwargs, {'existing_mailbox': 'test', 'replace': True})
            profile.write_text('existing profile must survive')
            with patch.object(module.sys, 'argv', args), patch.object(module, 'onboard') as submit, contextlib.redirect_stderr(out):
                self.assertEqual(module.main(), 1)
                submit.assert_not_called()
            self.assertEqual(profile.read_text(), 'existing profile must survive')


if __name__ == '__main__':
    unittest.main()
