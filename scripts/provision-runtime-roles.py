#!/usr/bin/env python3
"""Provision least-privilege roles only on the existing personal Relay database."""
import os
import re
import secrets
import stat
import subprocess
import tempfile
from pathlib import Path

root = Path(__file__).resolve().parents[1]
env_file = root / '.env'
try:
    descriptor = os.open(env_file, os.O_RDONLY | os.O_NOFOLLOW)
except OSError as error:
    raise SystemExit('Expected a private regular .env file; refusing to read credentials.') from error
with os.fdopen(descriptor) as source:
    info = os.fstat(source.fileno())
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise SystemExit('Expected a private regular .env file; refusing to read credentials.')
    raw = source.read()
values = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line and not line.startswith('#'))
changed = False
for name in ('RELAY_RUNTIME_DB_PASSWORD', 'RELAY_METRICS_DB_PASSWORD'):
    if name not in values:
        values[name] = secrets.token_hex(32)
        raw += '\n' + name + '=' + values[name] + '\n'
        changed = True
    if not re.fullmatch(r'[0-9a-f]{64}', values[name]):
        raise SystemExit('Expected generated hex credentials; refusing to replace existing values.')
if changed:
    descriptor, temporary = tempfile.mkstemp(prefix='.env-', dir=root)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, 'w') as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, env_file)
        directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
# Generated hex values cannot introduce SQL syntax; no secret is placed in argv.
sql = """
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='relay_runtime') THEN CREATE ROLE relay_runtime LOGIN; END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='relay_metrics') THEN CREATE ROLE relay_metrics LOGIN; END IF;
END $$;
"""
for role, variable in [('relay_runtime','RELAY_RUNTIME_DB_PASSWORD'),('relay_metrics','RELAY_METRICS_DB_PASSWORD')]:
    sql += f"ALTER ROLE {role} NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '{values[variable]}';\n"
sql += """
GRANT CONNECT ON DATABASE relay_dev TO relay_runtime, relay_metrics;
GRANT USAGE ON SCHEMA public TO relay_runtime, relay_metrics;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO relay_runtime;
GRANT INSERT, UPDATE ON destinations, events, deliveries, delivery_attempts, delivery_replays, signing_secrets, client_request_limits TO relay_runtime;
GRANT UPDATE ON clients TO relay_runtime;
GRANT SELECT ON schema_migrations, deliveries, delivery_attempts TO relay_metrics;
"""
try:
    subprocess.run(['docker','exec','-i','relay-dev-relay-db-1','psql','-U','relay_dev','-d','relay_dev','-v','ON_ERROR_STOP=1'], input=sql, text=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
except subprocess.CalledProcessError as error:
    raise SystemExit('Database role provisioning failed; no credentials were printed.') from error
print('Relay runtime and read-only metrics roles provisioned; credentials remain private.')
