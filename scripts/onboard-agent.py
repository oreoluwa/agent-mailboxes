#!/usr/bin/env python3
"""Onboard a provider mailbox and alias, then save a private portable agent profile."""
import argparse
import getpass
import importlib.util
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request
import warnings

_spec = importlib.util.spec_from_file_location('mailbox_onboard', Path(__file__).with_name('mailbox-onboard.py'))
entry = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(entry)


def api_origin(value):
    parsed = urllib.parse.urlsplit(value)
    if (not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ('', '/') or parsed.scheme not in ('http', 'https')):
        raise ValueError('API URL must be an HTTP(S) origin without credentials, path, query or fragment')
    if parsed.scheme == 'http' and parsed.hostname not in ('127.0.0.1', '::1'):
        raise ValueError('Use HTTPS for remote APIs; plain HTTP is allowed only on a loopback IP')
    return value.rstrip('/')


def onboard(base, master, config, password, alias, token_name, existing_mailbox=None, replace=False):
    base = api_origin(base)
    if existing_mailbox:
        route = '/v1/admin/agents'
        payload = dict(mailbox_name=existing_mailbox, alias_email=alias, token_name=token_name, replace=replace)
    else:
        route = '/v1/admin/onboard'
        payload = entry.validate_config(config)
        payload.update(password=password, alias_email=alias, token_name=token_name)
    request = urllib.request.Request(base + route, data=json.dumps(payload).encode(),
        headers={'Content-Type': 'application/json', 'X-Master-Key': master}, method='POST')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), entry.NoRedirect())
    try:
        with opener.open(request, timeout=60) as response:
            result = json.loads(response.read(65536))
    except urllib.error.HTTPError as error:
        code = error.code
        error.close()
        raise RuntimeError(f'Onboarding failed (HTTP {code}); no response body displayed') from None
    except Exception:
        raise RuntimeError('Onboarding result could not be confirmed. Check mailbox status before retrying.') from None
    if not isinstance(result, dict) or not result.get('token') or not result.get('mailbox_name'):
        raise RuntimeError('Server did not return an agent credential; check mailbox status before retrying')
    return {'version': 1, 'api_url': base, 'mailbox': result['mailbox_name'],
            'alias': result.get('alias_email', alias), 'token': result['token']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--api-url', default='http://127.0.0.1:8787')
    parser.add_argument('--master-key-file', type=Path, required=True)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument('--config', type=Path, help='New mailbox: non-secret provider settings JSON')
    source.add_argument('--existing-mailbox', help='Add or recover an agent on an existing mailbox; no provider password required')
    parser.add_argument('--replace', action='store_true', help='Explicitly revoke and replace the same named agent credential on the selected alias')
    parser.add_argument('--alias', required=True, help='Existing provider alias to assign to the agent')
    parser.add_argument('--token-name', default='agent')
    parser.add_argument('--profile', type=Path, required=True, help='New private output file; existing files are never replaced')
    args = parser.parse_args()
    output = None
    reserved = None
    succeeded = False
    try:
        base = api_origin(args.api_url)
        if args.replace and not args.existing_mailbox:
            raise ValueError('--replace requires --existing-mailbox')
        config = entry.validate_config(json.loads(args.config.read_text())) if args.config else None
        master = entry.read_private_file(args.master_key_file)
        if args.config and not sys.stdin.isatty():
            raise ValueError('Run in an interactive terminal; provider passwords cannot be piped')
        # Reserve the destination before any provider or database operation.
        output = os.open(args.profile, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        reserved = os.fstat(output)
        password = None
        if args.config:
            with warnings.catch_warnings():
                warnings.simplefilter('error', getpass.GetPassWarning)
                try:
                    password = getpass.getpass('Provider app password (hidden): ')
                except (getpass.GetPassWarning, EOFError):
                    raise ValueError('A terminal with echo suppression is required') from None
            if not password:
                raise ValueError('Password cannot be empty')
        profile = onboard(base, master, config, password, args.alias, args.token_name,
                          existing_mailbox=args.existing_mailbox, replace=args.replace)
        encoded = (json.dumps(profile, indent=2) + '\n').encode()
        with os.fdopen(output, 'wb') as handle:
            output = None
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        succeeded = True
        print('Agent access configured. Private agent profile saved to:', args.profile)
        print('Use: email --profile', args.profile, 'inbox list --unread --json')
        print('Provider passwords and operator credentials are not included in the profile.')
        return 0
    except KeyboardInterrupt:
        print('Onboarding cancelled. If a request was sent, use the existing-mailbox recovery flow.', file=sys.stderr)
    except (ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
    except OSError:
        print('Cannot read private inputs or create/write the profile. Existing output files are never overwritten.', file=sys.stderr)
    finally:
        if output is not None:
            os.close(output)
        if not succeeded and reserved is not None:
            try:
                current = args.profile.lstat()
                if (current.st_dev, current.st_ino, current.st_size) == (reserved.st_dev, reserved.st_ino, 0):
                    args.profile.unlink()
            except OSError:
                pass
        if not succeeded and reserved is not None:
            print('For an uncertain server result, recover with --existing-mailbox NAME --alias ADDRESS --token-name NAME --replace and a new/private profile path.', file=sys.stderr)
    return 1


if __name__ == '__main__':
    raise SystemExit(main())
