import argparse, json, os, pathlib, re, shutil, signal, socket, subprocess, sys, tempfile, time, urllib.request

parser = argparse.ArgumentParser()
parser.add_argument('binary')
args = parser.parse_args()
platform = sys.platform
root = pathlib.Path(tempfile.mkdtemp(prefix='router-service-%-$-'))
binary = root / 'router with spaces'
shutil.copy2(args.binary, binary)
label = 'test.codex-model-router.' + str(os.getpid())
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
config = root / 'config.json'
config.write_text(json.dumps({'listen': {'host': '127.0.0.1', 'port': port}, 'native': {'models': ['gpt-test']}, 'log': {'level': 'info'}}))
common = ['--config', str(config), '--bin', str(binary), '--label', label, '--throttle-interval', '1', '--working-directory', str(root), '--env', 'SMOKE_LITERAL=literal %n $HOME " slash\\']
if platform == 'darwin':
    common += ['--log-dir', str(root / 'logs')]
else:
    os.environ['XDG_RUNTIME_DIR'] = '/run/user/' + str(os.getuid())
unit = label + '.service'

def command(*argv):
    r = subprocess.run(argv, capture_output=True, text=True)
    if r.returncode:
        print(r.stderr, file=sys.stderr)
        r.check_returncode()
    return r.stdout

def service(action):
    return command(str(binary), 'service', action, *common)

def health():
    for _ in range(100):
        try:
            with urllib.request.urlopen('http://127.0.0.1:' + str(port) + '/healthz', timeout=1) as response:
                data = json.load(response)
            if data['ok']:
                return data
        except Exception:
            pass
        time.sleep(.2)
    raise RuntimeError('service did not become healthy')

def pid():
    if platform == 'darwin':
        state = command('launchctl', 'print', 'gui/' + str(os.getuid()) + '/' + label)
        return int(re.search(r'\bpid = (\d+)', state).group(1))
    return int(command('systemctl', '--user', 'show', unit, '--property=MainPID', '--value').strip())

installed = False
try:
    preview = service('preview')
    if platform != 'darwin':
        unitfile = root / unit
        unitfile.write_text(preview.split('# unit path:')[0])
        command('systemd-analyze', '--user', 'verify', str(unitfile))
    installed = True
    service('install')
    health()
    before = pid()
    if platform != 'darwin':
        environ = pathlib.Path('/proc/' + str(before) + '/environ').read_bytes().split(b'\0')
        assert b'SMOKE_LITERAL=literal %n $HOME " slash\\' in environ
    service('install')
    health()
    assert pid() != before
    before = pid()
    os.kill(before, signal.SIGKILL)
    for _ in range(100):
        try:
            after = pid()
            if after and after != before:
                break
        except Exception:
            pass
        time.sleep(.2)
    else:
        raise RuntimeError('crashed process was not restarted')
    health()
    status = service('status')
    assert 'health' in status
    if platform == 'darwin':
        log = (root / 'logs' / (label + '.err.log')).read_text()
        assert log.strip()
    else:
        log = command('journalctl', '--user', '-u', unit, '--no-pager', '-n', '30')
        assert 'router listening' in log or 'listening' in log
    service('uninstall')
    installed = False
    assert config.is_file()
    if platform == 'darwin':
        assert not (pathlib.Path.home() / 'Library/LaunchAgents' / (label + '.plist')).exists()
    else:
        assert not (pathlib.Path.home() / '.config/systemd/user' / unit).exists()
    print('PASS:', platform, 'install/reinstall/health/crash-recovery/logs/uninstall; literal paths and environment')
finally:
    if installed:
        try:
            service('uninstall')
            installed = False
        except Exception as exc:
            print('Cleanup failed; preserving', root, str(exc), file=sys.stderr)
    if not installed:
        shutil.rmtree(root)
