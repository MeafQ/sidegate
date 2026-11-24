# The names the lab shares with the daemon, each typed once. Sourced by lib.sh on the Mac and
# by the guest scripts in the VM. lab/names/names.go holds the lab programs' copy, and its test
# checks the two agree with each other and with the daemon.
LAB_HOME=/opt/sidegate-lab
RUN_DIR=/run/sidegate
DATA_DIR=/data/sidegate
HTTP_SOCKET=/run/sidegate-http/http.sock
BASE_PATH=/sidegate/
DAEMON_BIN=sidegated
CORE_UNIT=sidegate-core.service
HOOK_NAME=shared-runnable-sidegate.conf
AUTH_HEADER=X-Sidegate-Auth
PROOF_HEADER=X-Sidegate-Proof
CSRF_HEADER=X-Sidegate-Csrf
STATE_DB=state.db
METRICS_DB=metrics.db
BOOTSTRAP=bootstrap.json
NETWORK_KEY=network-key
NGINX_PROOF=nginx-proof
FEEDS=https://11.0.0.80/feed/
RELEASES=https://11.0.0.80/OWNER/sidegate/releases
LIFECYCLE_WORK=/var/tmp/sglc
