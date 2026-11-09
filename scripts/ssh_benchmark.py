#!/usr/bin/env python3
"""Real SSH stress test. Only mutates its own mktemp directory, never the source project."""
import argparse
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import time

parser=argparse.ArgumentParser()
parser.add_argument('host')
parser.add_argument('--files',type=int,default=2000)
args=parser.parse_args()
binary=str(Path(__file__).resolve().parent.parent/'bin/rsync_tool')
def ssh(command):
    p=subprocess.run(['ssh','-o','BatchMode=yes',args.host,command],capture_output=True,text=True,timeout=60)
    if p.returncode: raise RuntimeError(p.stderr)
    return p.stdout
remote=ssh('mktemp -d /tmp/rsync_tool-bench.XXXXXXXX').strip()
if not remote.startswith('/tmp/rsync_tool-bench.'):raise RuntimeError('invalid test directory')
local=Path(tempfile.mkdtemp(prefix='rsync_tool-ssh-bench-'))
results={'host':args.host,'files':args.files,'remote_fixture':remote,'interval_ms':100}
process=None
log=(local.parent/(local.name+'.log')).open('w')
def wait_for(check,timeout=90):
    until=time.monotonic()+timeout
    while time.monotonic()<until:
        if process is not None and process.poll() is not None:raise RuntimeError('sync exited; see '+log.name)
        if check():return
        time.sleep(.05)
    raise TimeoutError('benchmark timed out; see '+log.name)
def text(p):
    try:return p.read_text()
    except FileNotFoundError:return None
try:
    for n in range(args.files):
        p=local/'tree'/f'd{n//50:03d}'/f'f{n:05d}.txt'
        p.parent.mkdir(parents=True,exist_ok=True)
        p.write_text(f'initial {n}\n')
    (local/'empty'/'nested').mkdir(parents=True)
    started=time.monotonic()
    process=subprocess.Popen([binary,'start','--interval','100ms',args.host+':'+remote,str(local)],stdout=log,stderr=log)
    state_path=local/'.rsync_tool/state.json'
    def state():
        try:return json.loads(state_path.read_text())
        except (FileNotFoundError,json.JSONDecodeError):return {}
    wait_for(lambda:len(state().get('Base',{}))>=args.files and not state().get('LastError'))
    results['initial_sync_including_bootstrap_seconds']=round(time.monotonic()-started,3)
    # Remote confirmation excludes SSH setup time from the recorded synchronization interval.
    assert int(ssh('find '+shlex.quote(remote+'/tree')+' -type f | wc -l').strip())==args.files
    assert ssh('test -d '+shlex.quote(remote+'/empty/nested')+' && echo yes').strip()=='yes'
    started=time.monotonic()
    for n in range(100):
        (local/'hot.txt').write_text(f'edit {n}\n')
        time.sleep(.005)
    final='edit 99\n'
    def confirmed_hot():
        s=state();v=s.get('Base',{}).get('hot.txt');return v and v['Hash']==__import__('hashlib').sha256(final.encode()).hexdigest()
    wait_for(confirmed_hot)
    results['100_edits_5ms_apart_converged_seconds']=round(time.monotonic()-started,3)
    assert ssh('cat '+shlex.quote(remote+'/hot.txt'))==final
    started=time.monotonic()
    # Verify coalesced creation followed by recursive deletion and empty directory operations.
    shutil.rmtree(local/'tree')
    shutil.rmtree(local/'empty')
    (local/'newempty'/'nested').mkdir(parents=True)
    wait_for(lambda:not any(p=='tree' or p.startswith('tree/') or p=='empty' or p.startswith('empty/') for p in state().get('Base',{})) and 'newempty/nested' in state().get('Base',{}) and not state().get('LastError'))
    results['recursive_delete_seconds']=round(time.monotonic()-started,3)
    assert ssh('test ! -e '+shlex.quote(remote+'/tree')+' && test ! -e '+shlex.quote(remote+'/empty')+' && test -d '+shlex.quote(remote+'/newempty/nested')+' && echo yes').strip()=='yes'
    started=time.monotonic()
    ssh('mkdir -p '+shlex.quote(remote+'/remote-created/deep')+'; printf remote-change > '+shlex.quote(remote+'/remote-created/deep/a.txt'))
    wait_for(lambda:text(local/'remote-created/deep/a.txt')=='remote-change')
    results['remote_create_to_local_seconds_including_ssh_command']=round(time.monotonic()-started,3)
    (local/'last.txt').write_text('stop flush')
    started=time.monotonic()
    stopped=subprocess.run([binary,'stop',str(local)],capture_output=True,text=True,timeout=60)
    if stopped.returncode:raise RuntimeError(stopped.stderr)
    assert process.wait(timeout=20)==0
    assert ssh('cat '+shlex.quote(remote+'/last.txt'))=='stop flush'
    results['stop_flush_seconds']=round(time.monotonic()-started,3)
    results['passed']=True
    print(json.dumps(results,ensure_ascii=False,indent=2))
finally:
    if process is not None and process.poll() is None:
        process.terminate()
        try:process.wait(timeout=30)
        except subprocess.TimeoutExpired:process.kill();process.wait()
    log.close()
    ssh('rm -rf -- '+shlex.quote(remote))
    shutil.rmtree(local)
