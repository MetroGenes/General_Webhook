#!/usr/bin/env bash
# Run the real Uptime Kuma contract test. Requires Docker and Go on the host;
# Node.js and socket.io-client are provided by the disposable Kuma image.
set -euo pipefail
umask 077

command -v docker >/dev/null
command -v go >/dev/null
kuma_repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
kuma_test_dir=$(mktemp -d "${TMPDIR:-/tmp}/general-webhook-kuma.XXXXXX")
kuma_test_container="general-webhook-kuma-${kuma_test_dir##*.}"
kuma_container_id=""
kuma_image="${KUMA_IMAGE:-louislam/uptime-kuma:2@sha256:3e24e96c89efff0e3a4b0698cbdd36c15ad3022371db57166e5588853002ee5c}"

cleanup() {
    local kuma_exit=$?
    trap - EXIT INT TERM
    if [[ -n "$kuma_container_id" ]]; then
        if [[ -n "${KUMA_TEST_LOG_DIR:-}" ]]; then
            docker logs "$kuma_container_id" > "$KUMA_TEST_LOG_DIR/kuma-container.log" 2>&1 || true
        fi
        if ! docker rm -f "$kuma_container_id" >/dev/null 2>&1; then
            printf 'Could not remove test container %s\n' "$kuma_test_container" >&2
            kuma_exit=1
        fi
    fi
    rm -rf -- "$kuma_test_dir"
    exit "$kuma_exit"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -n "${KUMA_TEST_LOG_DIR:-}" ]]; then
    mkdir -p -- "$KUMA_TEST_LOG_DIR"
fi

kuma_container_id=$(docker create --name "$kuma_test_container" \
    --publish 127.0.0.1::3001 --memory 512m \
    --tmpfs /app/data:rw,size=128m \
    --env UPTIME_KUMA_DB_TYPE=sqlite "$kuma_image")
docker start "$kuma_container_id" >/dev/null
printf 'Started disposable Uptime Kuma container %s\n' "$kuma_test_container"

# Credentials are generated solely for this temporary, loopback-only fixture.
# Wait for loginRequired: emitting an RPC immediately on connect can precede
# Kuma's asynchronous installation of its socket handlers.
docker exec -i "$kuma_test_container" node > "$kuma_test_dir/fixture.json" <<'NODE'
const { io } = require("socket.io-client");
const crypto = require("crypto");
const { setTimeout: delay } = require("timers/promises");
let socket;
const watchdog = setTimeout(() => {
    console.error("Kuma fixture setup timed out");
    process.exit(1);
}, 90000);

async function main() {
    const until = Date.now() + 60000;
    for (;;) {
        try {
            const response = await fetch("http://127.0.0.1:3001/setup-database-info", {
                signal: AbortSignal.timeout(2000),
            });
            const state = await response.json();
            if (response.ok && state.needSetup === false && state.runningSetup === false) {
                break;
            }
        } catch {}
        if (Date.now() >= until) {
            throw new Error("Kuma server did not finish database initialization");
        }
        await delay(500);
    }
    socket = io("http://127.0.0.1:3001", {
        transports: ["websocket"], reconnection: false, timeout: 10000,
    });
    await new Promise((resolve, reject) => {
        socket.once("loginRequired", resolve);
        socket.once("connect_error", reject);
    });
    const rpc = (event, ...args) => new Promise((resolve, reject) => {
        socket.timeout(10000).emit(event, ...args, (error, result) => {
            if (error) reject(error); else resolve(result);
        });
    });
    if (await rpc("needSetup") !== true) {
        throw new Error("Refusing to modify a Kuma instance that already has a user");
    }
    const username = "general-webhook-integration";
    const password = "Gw-Kuma-Test!" + crypto.randomBytes(24).toString("hex");
    const setup = await rpc("setup", username, password);
    if (!setup.ok) throw new Error("Kuma user setup failed");
    const login = await rpc("login", { username, password });
    if (!login.ok) throw new Error("Kuma fixture login failed");

    const intervalSeconds = 5;
    const token = crypto.randomBytes(24).toString("hex");
    const inactiveToken = crypto.randomBytes(24).toString("hex");
    async function addMonitor(name, pushToken, active) {
        const result = await rpc("add", {
            name, type: "push", pushToken, active,
            interval: intervalSeconds, retryInterval: intervalSeconds,
            maxretries: 0, resendInterval: 0, notificationIDList: {},
            accepted_statuscodes: ["200-299"], upsideDown: false,
            conditions: [], kafkaProducerBrokers: [], kafkaProducerSaslOptions: {},
            rabbitmqNodes: [],
        });
        if (!result.ok) throw new Error("Kuma push-monitor creation failed");
        return result.monitorID;
    }
    const monitorId = await addMonitor("general-webhook-active-test", token, true);
    const inactiveMonitorId = await addMonitor("general-webhook-inactive-test", inactiveToken, false);
    console.log(JSON.stringify({
        version: require("./package.json").version,
        monitorId, inactiveMonitorId, intervalSeconds, token, inactiveToken,
    }));
}

main().catch(error => {
    console.error(error.message);
    process.exitCode = 1;
}).finally(() => {
    if (socket) socket.close();
    clearTimeout(watchdog);
});
NODE

kuma_port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "3001/tcp") 0).HostPort}}' "$kuma_test_container")
export KUMA_TEST_CONTAINER="$kuma_test_container"
export KUMA_TEST_BASE_URL="http://127.0.0.1:$kuma_port"
export KUMA_TEST_FIXTURE="$kuma_test_dir/fixture.json"
cd -- "$kuma_repo_root"
if [[ -n "${KUMA_TEST_LOG_DIR:-}" ]]; then
    go test -tags=kuma_integration -run '^TestUptimeKumaIntegration$' -count=1 -timeout=90s -v ./internal/heartbeat \
        2>&1 | tee "$KUMA_TEST_LOG_DIR/kuma-integration.log"
else
    go test -tags=kuma_integration -run '^TestUptimeKumaIntegration$' -count=1 -timeout=90s -v ./internal/heartbeat
fi
