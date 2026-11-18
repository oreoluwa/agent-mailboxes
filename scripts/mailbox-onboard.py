#!/usr/bin/env python3
"""Enter a provider app password locally and submit it to a loopback emaild API."""
import argparse
import getpass
import json
import os
from pathlib import Path
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request
import warnings


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_private_file(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'r') as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid():
            raise ValueError('Secret file must be owned by this user, regular, and mode 0600 or stricter')
        value = handle.read(16385).strip()
        if not value or len(value) > 16384:
            raise ValueError('Secret file is empty or too large')
        return value


def validate_config(config):
    fields = {'name', 'username', 'imap_host', 'imap_port', 'imap_tls', 'smtp_host', 'smtp_port', 'smtp_tls'}
    if not isinstance(config, dict) or set(config) != fields:
        raise ValueError('Config must contain exactly the documented non-secret fields; no password')
    for field in ('name', 'username', 'imap_host', 'smtp_host'):
        if not isinstance(config[field], str) or not config[field].strip():
            raise ValueError('Config contains an empty or invalid field')
    for field in ('imap_port', 'smtp_port'):
        if type(config[field]) is not int or not 1 <= config[field] <= 65535:
            raise ValueError('Invalid provider port')
    if config['imap_tls'] is not True or config['smtp_tls'] is not True:
        raise ValueError('This helper requires implicit TLS for both provider connections')
    return dict(config)


def endpoint(base):
    url = urllib.parse.urlsplit(base)
    if (url.scheme != 'http' or url.hostname not in ('127.0.0.1', '::1')
            or url.username or url.password or url.query or url.fragment or url.path not in ('', '/')):
        raise ValueError('API must be an HTTP loopback IP origin without credentials, path, query or fragment')
    return base.rstrip('/') + '/v1/admin/mailboxes'


def submit(base, key, config, password):
    payload = validate_config(config)
    payload['password'] = password
    request = urllib.request.Request(endpoint(base), data=json.dumps(payload).encode(), method='POST',
        headers={'Content-Type': 'application/json', 'X-Master-Key': key})
    # Never forward the app password through environment-configured proxies or redirects.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=45) as response:
            result = json.loads(response.read(65536))
    except urllib.error.HTTPError as error:
        raise RuntimeError(f'Onboarding failed (HTTP {error.code}); response body withheld') from None
    except Exception:
        raise RuntimeError('Onboarding connection or response failed; details withheld') from None
    if result.get('status') != 'ok':
        raise RuntimeError('Onboarding did not confirm success; response withheld')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--api-url', default='http://127.0.0.1:8787')
    parser.add_argument('--master-key-file', required=True, type=Path)
    parser.add_argument('--config', required=True, type=Path, help='Non-secret provider JSON; see docs/supported-onboarding.md')
    args = parser.parse_args()
    try:
        endpoint(args.api_url)
        config = validate_config(json.loads(args.config.read_text()))
        key = read_private_file(args.master_key_file)
        if not sys.stdin.isatty():
            raise ValueError('Run this helper in an interactive terminal; passwords cannot be piped')
        with warnings.catch_warnings():
            warnings.simplefilter('error', getpass.GetPassWarning)
            try:
                password = getpass.getpass('Provider app password (hidden): ')
            except (getpass.GetPassWarning, EOFError):
                raise ValueError('A terminal with echo suppression is required') from None
        if not password:
            raise ValueError('Password cannot be empty')
        submit(args.api_url, key, config, password)
        print('Mailbox credentials validated and stored encrypted. No test email was sent.')
    except (ValueError, RuntimeError, OSError, json.JSONDecodeError) as error:
        # File/JSON/parser errors may contain user data; only controlled messages are printed.
        if type(error) in (ValueError, RuntimeError):
            print(str(error), file=sys.stderr)
        else:
            print('Unable to read local configuration or secret file', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
