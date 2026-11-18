#!/usr/bin/env python3
"""Verify generated persistent setup survives recreation using synthetic data only."""
import importlib.util
from pathlib import Path
import socket
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location('setup_local', Path(__file__).with_name('setup-local.py'))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


def main():
    checkout = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix='agent-emails-persistence-test-') as tmp:
        directory = Path(tmp) / 'installation'
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        manifest = setup.init_installation(directory, checkout, port)
        compose = ['docker', 'compose', '--env-file', '/dev/null', '--project-name', manifest['project_name'], '--file', str(directory / 'compose.yml')]
        def command(args, **kwargs):
            return subprocess.run(compose + args, cwd=directory, check=True, **kwargs)
        try:
            command(['up', '-d', '--build', '--wait'])
            command(['exec', '-T', 'postgres', 'psql', '-U', 'email', '-d', 'agent_emails', '-v', 'ON_ERROR_STOP=1', '-c',
                     'CREATE TABLE persistence_probe(value text); INSERT INTO persistence_probe VALUES (\'synthetic-retained\');'])
            command(['down'])
            command(['up', '-d', '--wait'])
            result = command(['exec', '-T', 'postgres', 'psql', '-U', 'email', '-d', 'agent_emails', '-At', '-c',
                              "SELECT value FROM persistence_probe"], capture_output=True, text=True)
            if result.stdout.strip() != 'synthetic-retained':
                raise RuntimeError('Persistent volume did not retain synthetic data')
            print('PASS: generated service healthy; named-volume data survived container recreation')
        finally:
            # This unique installation and its volume were created only by this test.
            subprocess.run(compose + ['down', '--volumes'], cwd=directory, check=False)


if __name__ == '__main__':
    main()
