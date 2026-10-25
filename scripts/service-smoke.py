#!/usr/bin/env python3
"""Run the README source install and live LaunchAgent check in a disposable HOME.

Requires macOS, Go, curl, and TYPESAFE_API_KEY. No real config is read or changed.
Run from the repository root: python3 scripts/service-smoke.py /tmp/brout-evidence
The output directory keeps only the transcript and sanitized responses.
"""

from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import select
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile


LABEL = "io.github.victorarias.brout"
TARGET = f"gui/{os.getuid()}/{LABEL}"
ROOT = Path(__file__).resolve().parent.parent


@contextmanager
def disposable_home():
    home = tempfile.mkdtemp(prefix="brout smoke ")
    try:
        yield home
    except BaseException:
        binary = str(Path(home) / ".local/bin/brout")
        print(f"Preserved temporary HOME for recovery: {home}", file=sys.stderr, flush=True)
        print("If an agent remains, clean up with: " +
              shlex.join(["env", f"HOME={home}", binary, "service", "uninstall"]),
              file=sys.stderr, flush=True)
        raise
    else:
        # The caller verifies both registration and PID absence before exit.
        shutil.rmtree(home)


def main():
    if sys.platform != "darwin":
        raise SystemExit("this check requires macOS")
    key = os.environ.get("TYPESAFE_API_KEY")
    if not key:
        raise SystemExit("set TYPESAFE_API_KEY before running this check")
    evidence = Path(sys.argv[1]).resolve()
    evidence.mkdir(parents=True, exist_ok=True)

    def run(args, env=None, check=True):
        # Commands contain paths and request bodies, never keys.
        print("$ " + " ".join(map(str, args)), flush=True)
        result = subprocess.run(args, env=env, cwd=ROOT, text=True, capture_output=True)
        print(result.stdout + result.stderr, end="", flush=True)
        if check and result.returncode:
            raise RuntimeError(f"command exited {result.returncode}")
        return result

    existing = run(["launchctl", "print", TARGET], check=False)
    if existing.returncode != 113:
        raise SystemExit("refusing to touch an existing agent or an inaccessible launchd domain")
    real_plist = Path.home() / "Library/LaunchAgents" / (LABEL + ".plist")
    if real_plist.exists():
        raise SystemExit(f"refusing to run while {real_plist} exists")

    # Ask the OS for an unused non-default port. The service binds the same port
    # after this socket closes; a collision fails visibly rather than replacing it.
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    address = f"http://127.0.0.1:{port}"

    with disposable_home() as temp:
        home = Path(temp)
        env = os.environ.copy()
        env.update(HOME=temp, XDG_CONFIG_HOME="", XDG_STATE_HOME="")
        binary = home / ".local/bin/brout"
        binary.parent.mkdir(parents=True)
        # Keep Go's caches outside the disposable HOME without reading app data.
        # Resolve Go before changing HOME so shell version-manager shims do not
        # read their own config as a fresh user.
        go_root = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
        go = str(Path(go_root) / "bin/go")
        env["GOROOT"] = go_root
        for name in ("GOCACHE", "GOMODCACHE", "GOPATH"):
            env[name] = subprocess.check_output([go, "env", name], text=True).strip()
        run([go, "build", "-o", str(binary), "./cmd/brout"], env)
        run([str(binary), "help"], env)
        config = home / ".config/brout/config.toml"
        config.parent.mkdir(parents=True)
        config.write_text(f'listen = "127.0.0.1:{port}"\nrouting = ["typesafe/jev-latest"]\n'
                          f'attempt_timeout = "4s"\n[providers.typesafe]\napi_key = {json.dumps(key)}\n'
                          '[log]\nenabled = true\n')
        config.chmod(0o600)
        print(f"wrote README config with inline key (redacted), listen={address}", flush=True)
        request = {
            "state": "I was charged twice. Please fix this today.",
            "questions": {
                "billing": {"type": "noul", "instructions": "Is this about billing?"},
                "tone": {"type": "choice", "instructions": "What is the customer tone?",
                         "criteria": {"calm": None, "frustrated": None}},
                "urgency": {"type": "score", "instructions": "How urgent is this?",
                            "criteria": ["can wait", "this week", "today"]},
            },
        }

        def answer(name):
            result = run(["curl", "--fail-with-body", "--silent", "--show-error", "-i",
                          address + "/v1/systemone", "-H", "Content-Type: application/json",
                          "-d", json.dumps(request)], env)
            (evidence / f"{name}.txt").write_text(result.stdout)
            headers, body = result.stdout.split("\n\n", 1)
            response = json.loads(body)
            assert response["model"].startswith("typesafe/")
            assert set(response["answers"]) == set(request["questions"])
            return re.search(r"(?im)^Brout-Request-Id: (.+)$", headers).group(1).strip()

        foreground = subprocess.Popen([str(binary), "serve"], env=env, stdout=subprocess.PIPE,
                                      stderr=subprocess.STDOUT, text=True)
        try:
            while True:
                line = foreground.stdout.readline()
                if not line:
                    raise RuntimeError("foreground gateway exited before listening")
                print(line, end="", flush=True)
                if "brout listening on" in line:
                    break
            request_id = answer("foreground-answer")
            run([str(binary), "logs", "--limit", "10"], env)
            run([str(binary), "logs", "show", request_id], env)
            run([str(binary), "replay", request_id], env)
        finally:
            foreground.send_signal(signal.SIGTERM)
            foreground.wait()
        assert foreground.returncode == 0
        print(f"foreground PID {foreground.pid} exited {foreground.returncode}", flush=True)

        bad_config = home / "unset.toml"
        bad_config.write_text('[providers.typesafe]\napi_key_env = "BROUT_SMOKE_UNSET_KEY"\n')
        env.pop("BROUT_SMOKE_UNSET_KEY", None)
        failure = run([str(binary), "serve", "--config", str(bad_config)], env, check=False)
        assert failure.returncode != 0
        assert "BROUT_SMOKE_UNSET_KEY, which is not set" in failure.stderr
        assert "put the key in the config as api_key" in failure.stderr
        failure = run([str(binary), "service", "install", "--config", str(bad_config)], env, check=False)
        assert failure.returncode != 0
        assert "launchd does not inherit your shell environment" in failure.stderr

        state = home / ".local/state/brout"
        state.mkdir(parents=True, exist_ok=True)
        # Watch actual startup writes with kqueue, rather than sleeps or probes.
        streams = [(state / name).open("a+")
                   for name in ("service.stdout.log", "service.stderr.log")]
        queue = select.kqueue()
        queue.control([select.kevent(f.fileno(), filter=select.KQ_FILTER_VNODE,
                                     flags=select.KQ_EV_ADD | select.KQ_EV_CLEAR,
                                     fflags=select.KQ_NOTE_WRITE) for f in streams], 0)
        installed = False
        pid = None
        try:
            run([str(binary), "service", "status"], env)
            # Set before bootstrap so a partial install also gets cleanup.
            installed = True
            run([str(binary), "service", "install"], env)
            while True:
                stdout = streams[0].read()
                stderr = streams[1].read()
                print(stdout + stderr, end="", flush=True)
                if stderr:
                    raise RuntimeError("LaunchAgent printed a startup error")
                if "brout listening on" in stdout:
                    break
                queue.control(None, len(streams))
            status = run(["launchctl", "print", TARGET])
            pid = int(re.search(r"(?m)^\s*pid = (\d+)$", status.stdout).group(1))
            print(f"captured LaunchAgent PID {pid}", flush=True)
            run([str(binary), "service", "status"], env)
            answer("launchagent-answer")
            run(["curl", "--fail-with-body", "--silent", "--show-error", address + "/v1/models"], env)
        finally:
            if installed:
                run([str(binary), "service", "uninstall"], env)
            for stream in streams:
                stream.close()
            queue.close()

        plist = home / "Library/LaunchAgents" / (LABEL + ".plist")
        assert not plist.exists(), f"plist remains: {plist}"
        if pid is not None:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                print(f"LaunchAgent PID {pid} no longer exists", flush=True)
            else:
                raise RuntimeError(f"LaunchAgent PID {pid} still exists")
        run([str(binary), "service", "status"], env)
        final = run(["launchctl", "print", TARGET], check=False)
        assert final.returncode == 113
        assert not real_plist.exists()
        print("PASS: README answer, live LaunchAgent answer, F8 error, no plist or process", flush=True)


if __name__ == "__main__":
    main()
