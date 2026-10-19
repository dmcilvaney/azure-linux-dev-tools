#!/bin/bash
set -euo pipefail

cd /workdir/project
find /workdir/fixture -type f -print0 | sort -z | xargs -0 sha256sum >/artifacts/source-before
if rpm -q jq >/dev/null 2>&1; then
    echo "jq must be absent before TMT installs the declared dependency" >&2
    exit 1
fi

for case in pass mixed skip fail warn with.dots empty prepare-failure finish-failure report-failure multi; do
    plan="/plans/${case}$"
    if [[ "${case}" == multi ]]; then
        plan='/plans/(pass|skip)$'
    fi
    set +e
    TMT_STATE_FORMAT=json azldev --no-default-config component test azldev-tmt-probe \
        --provision local --source-dir /workdir/fixture --work-dir /artifacts/work \
        --plan "${plan}" --rpm /opt/azldev-tmt-fixtures/candidate.rpm -O json \
        >"/artifacts/${case}.json" 2>"/artifacts/${case}.stderr"
    status=$?
    set -e
    printf '%s\n' "${status}" >"/artifacts/${case}.exit"
done

root="$(jq -r .artifactRoot /artifacts/pass.json)"
run_state="$(jq -r '.invocations[0].nativeArtifacts[] | select(.format == "tmt-run") | .path' /artifacts/pass.json)"
native="${root}/${run_state%/run.yaml}"
python=/artifacts/work/tmt/venv/bin/python3

# Exercise every projection and duplicate test identities with native saved records.
cp -a "${native}" /artifacts/projection-native
"${python}" - <<'PY'
import copy
from pathlib import Path
from tmt.utils import to_yaml, yaml_to_list

path = Path("/artifacts/projection-native/plans/pass/execute/results.yaml")
original = yaml_to_list(path.read_text())[0]
results = []
for index, outcome in enumerate(("pass", "fail", "skip", "info", "error", "warn", "pending")):
    result = copy.deepcopy(original)
    result.update({"result": outcome, "original-result": outcome,
                   "serial-number": index // 2 + 1, "log": [], "check": [], "subresult": []})
    result["guest"]["name"] = "client" if index % 2 == 0 else "server"
    results.append(result)
path.write_text(to_yaml(results))
PY
find /artifacts/projection-native -type f -print0 | sort -z | xargs -0 sha256sum >/artifacts/native-before
"${python}" "${root}/export-junit.py" /artifacts/projection-native/plans/pass/execute/results.yaml /plans/pass \
    /artifacts/projection.xml "${root}/junit.xml.j2"
find /artifacts/projection-native -type f -print0 | sort -z | xargs -0 sha256sum >/artifacts/native-after
cmp /artifacts/native-before /artifacts/native-after

# A controlled runner subprocess verifies signal handling, not guest teardown.
export AZL_TMT_SIGNAL_FIXTURE="${native}"
mv /artifacts/work/tmt/venv/bin/tmt /artifacts/work/tmt/venv/bin/tmt-original
cat >/artifacts/work/tmt/venv/bin/tmt <<'SH'
#!/bin/bash
set -eu
while [[ "$1" != --id ]]; do shift; done
cp -a "${AZL_TMT_SIGNAL_FIXTURE}" "$2"
sed -i 's/status: done/status: todo/' "$2/plans/pass/execute/step.yaml"
echo interrupted-runner-stdout
echo interrupted-runner-stderr >&2
if [[ -n "${AZL_TMT_CANCEL_READY:-}" ]]; then
    exec /artifacts/work/tmt/venv/bin/python3 -c \
        'import os, time; from pathlib import Path; Path(os.environ["AZL_TMT_CANCEL_READY"]).write_text("ready"); time.sleep(60)'
fi
kill -TERM "$$"
SH
chmod 0755 /artifacts/work/tmt/venv/bin/tmt
set +e
azldev --no-default-config component test azldev-tmt-probe \
    --provision local --source-dir /workdir/fixture --work-dir /artifacts/work \
    --plan /plans/pass --rpm /opt/azldev-tmt-fixtures/candidate.rpm -O json \
    >/artifacts/interrupted.json 2>/artifacts/interrupted.stderr
status=$?
set -e
printf '%s\n' "${status}" >/artifacts/interrupted.exit

# Signal azldev itself only after the controlled runner has persisted its evidence.
"${python}" - <<'PY'
import os
import signal
import subprocess
import time
from pathlib import Path

for sig in (signal.SIGINT, signal.SIGTERM):
    name = f"cancelled-{sig.name.lower()}"
    ready = Path(f"/artifacts/{name}.ready")
    with Path(f"/artifacts/{name}.json").open("w") as stdout, \
            Path(f"/artifacts/{name}.stderr").open("w") as stderr:
        process = subprocess.Popen(
            ["azldev", "--no-default-config", "component", "test", "azldev-tmt-probe",
             "--provision", "local", "--source-dir", "/workdir/fixture",
             "--work-dir", "/artifacts/work", "--plan", "/plans/pass",
             "--rpm", "/opt/azldev-tmt-fixtures/candidate.rpm", "-O", "json"],
            stdout=stdout, stderr=stderr,
            env={**os.environ, "AZL_TMT_CANCEL_READY": str(ready)},
        )
        try:
            deadline = time.monotonic() + 30
            while not ready.exists():
                if process.poll() is not None:
                    raise RuntimeError(f"{name}: azldev exited before runner readiness")
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"{name}: runner readiness timed out")
                time.sleep(0.05)
            process.send_signal(sig)
            status = process.wait(timeout=10)
            Path(f"/artifacts/{name}.exit").write_text(f"{status}\n")
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=10)
PY
mv /artifacts/work/tmt/venv/bin/tmt-original /artifacts/work/tmt/venv/bin/tmt

find /workdir/fixture -type f -print0 | sort -z | xargs -0 sha256sum >/artifacts/source-after
cmp /artifacts/source-before /artifacts/source-after
rpm -q jq
