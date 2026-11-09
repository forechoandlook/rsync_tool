#!/usr/bin/env python3
"""Exercise the built CLI and SSH shell/protocol using a local SSH stand-in.
This does not validate authentication or a real SSH server.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import time

BINARY = str(Path(__file__).resolve().parent.parent / 'bin' / 'rsync_tool')
with tempfile.TemporaryDirectory(prefix='rsync_tool-integration-') as tmp:
    root = Path(tmp)
    local, remote, fakebin = (root / x for x in ('local', "remote with ' space", 'fakebin'))
    for p in (local, remote, fakebin):
        p.mkdir()
    ssh = fakebin / 'ssh'
    ssh.write_text('#!/bin/sh\nfor arg do command=$arg; done\nexec /bin/sh -c "$command"\n')
    ssh.chmod(0o755)
    env = dict(os.environ, PATH=str(fakebin) + ':' + os.environ['PATH'])
    spec = 'test-host:' + str(remote)
    def run(*args, success=True):
        result = subprocess.run([BINARY, *map(str, args)], env=env, text=True, capture_output=True, timeout=40)
        if success and result.returncode:
            raise AssertionError(result.stdout + result.stderr)
        if not success and not result.returncode:
            raise AssertionError('expected failure: ' + str(args))
        return result
    def wait_for(check):
        until = time.monotonic() + 20
        while time.monotonic() < until:
            if check():
                return
            time.sleep(.05)
        raise AssertionError('timed out waiting for sync')
    def content(p):
        return p.read_text() if p.exists() else None

    (remote / 'code.txt').write_text('remote initial')
    (remote / '.gitignore').write_text('*.cache\n')
    (remote / 'ignored.cache').write_text('private')
    before = set(Path('/tmp').glob('rsync_tool.????????'))
    run('sync', spec, local)
    assert content(local / 'code.txt') == 'remote initial'
    assert not (local / 'ignored.cache').exists()
    assert set(Path('/tmp').glob('rsync_tool.????????')) == before, 'helper not cleaned up'
    (local / 'code.txt').write_text('agent edit')
    (remote / 'code.txt').write_text('remote edit')
    run('sync', spec, local, success=False)
    assert content(local / 'code.txt') == 'agent edit'
    assert len(list((local / '.rsync_tool/conflicts').glob('code.txt.*'))) == 2
    run('sync', spec, local, '-f', 'push')
    assert content(remote / 'code.txt') == 'agent edit'
    log = (root / 'watch.log').open('w')
    process = subprocess.Popen([BINARY, 'start', '--interval', '100ms', spec, str(local)], env=env, stdout=log, stderr=log)
    try:
        wait_for(lambda: 'running: true' in run('status', local).stdout)
        (local / 'new.txt').write_text('new local')
        wait_for(lambda: content(remote / 'new.txt') == 'new local')
        (remote / 'new.txt').write_text('new remote')
        wait_for(lambda: content(local / 'new.txt') == 'new remote')
        (local / 'code.txt').write_text('last write before stop')
        run('stop', local)
        assert process.wait(timeout=20) == 0, (root / 'watch.log').read_text()
        assert content(remote / 'code.txt') == 'last write before stop'
        assert 'running: false' in run('status', local).stdout
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait(timeout=20)
        log.close()
    assert set(Path('/tmp').glob('rsync_tool.????????')) == before, 'watch helper not cleaned up'
    print('PASS: helper upload/cleanup, SSH protocol, conflict, force, watch, stop flush, status')
