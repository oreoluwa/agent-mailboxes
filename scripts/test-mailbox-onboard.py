#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


onboard = module('mailbox-onboard')


class CredentialEntryTests(unittest.TestCase):
    def test_loopback_only(self):
        for base in ('https://example.com', 'http://localhost', 'http://127.0.0.1.evil',
                     'http://user@127.0.0.1', 'http://127.0.0.1/path'):
            with self.assertRaises(ValueError):
                onboard.endpoint(base)
        self.assertEqual(onboard.endpoint('http://127.0.0.1:1234'), 'http://127.0.0.1:1234/v1/admin/mailboxes')

    def test_private_file_permissions(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp) / 'master.key'
            key.write_text('synthetic-test-key')
            key.chmod(0o600)
            self.assertTrue(onboard.read_private_file(key))
            key.chmod(0o644)
            with self.assertRaises(ValueError):
                onboard.read_private_file(key)

    def test_config_cannot_supply_password_or_plaintext_transport(self):
        config = dict(name='pilot', username='user@example.test', imap_host='imap.example.test',
                      imap_port=993, imap_tls=True, smtp_host='smtp.example.test', smtp_port=465, smtp_tls=True)
        with self.assertRaises(ValueError):
            onboard.validate_config(dict(config, password='secret'))
        with self.assertRaises(ValueError):
            onboard.validate_config(dict(config, smtp_tls=False))
        with patch.object(onboard.urllib.request, 'build_opener') as factory:
            factory.return_value.open.side_effect = urllib.error.HTTPError('url', 400, 'secret echoed', {}, None)
            with self.assertRaisesRegex(RuntimeError, 'response body withheld') as failure:
                onboard.submit('http://127.0.0.1:1234', 'master', config, 'secret')
            self.assertNotIn('secret', str(failure.exception))
        self.assertIsNone(onboard.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://elsewhere'))


if __name__ == '__main__':
    unittest.main()
