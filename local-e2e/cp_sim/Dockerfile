# Python tenant-CP simulator + canary test driver.
# Single image — pytest + httpx + the canary tests baked in.
FROM python:3.11-slim@sha256:e78299e55776ca065dcb769f80161f48465ad352014240eb5fe4712e22505e9b

WORKDIR /harness

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Test files are bind-mounted by docker-compose at run time so a `pytest -x`
# rerun loop doesn't require a rebuild. The COPY here is for the
# self-contained image used by Gitea Actions (where bind mounts are awkward).
COPY cp_sim.py /harness/cp_sim.py
COPY canary /harness/canary

ENV PYTHONUNBUFFERED=1

# Default: run the 4 canaries with verbose output + JUnit XML for CI.
CMD ["pytest", "-v", "--tb=short", "--junitxml=/harness/artifacts/junit.xml", "canary/"]
