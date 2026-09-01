#!/bin/bash
# Box-day validation battery for the containerized ukp stack.
# Run from hack/runtime-container/ on the bare-metal box, after
# box/host-bootstrap.sh and after building/loading the ukp-runtime:proto
# image. Requires the real AGENT_PULL_* credentials in the active ukp.conf
# for image pulls (Phase B onward).
#
# Usage: box/validate.sh [phase]   (default: all)
set -u
cd "$(dirname "$0")/.."

TOKEN="bG9jYWw6bG9jYWx0ZXN0"   # matches seed.sh users.json
API="http://127.0.0.1:45232/v1"
PASS=0; FAIL=0

api() { docker compose exec -T netbase curl -s -m 10 -H "Authorization: Bearer ${TOKEN}" "$@"; }
check() { # name condition-command...
	local name="$1"; shift
	if "$@" >/dev/null 2>&1; then echo "PASS: ${name}"; PASS=$((PASS+1));
	else echo "FAIL: ${name}"; FAIL=$((FAIL+1)); fi
}

phase_a_bringup() {
	echo "=== Phase A: bringup (with firewall) ==="
	docker compose --profile firewall up -d
	echo "waiting for ukpd health..."
	for i in $(seq 1 60); do
		state=$(docker compose ps ukpd --format '{{.Health}}' 2>/dev/null)
		[ "$state" = "healthy" ] && break
		sleep 2
	done
	check "ukpd healthy" test "$state" = healthy
	check "api reachable" api -f "${API}/instances"
	check "coredns up"    docker compose exec -T netbase sh -c 'ss -lnu | grep -q ":53 "'
	check "openresty up"  docker compose exec -T netbase sh -c 'ss -lnt | grep -q ":443 "'
	check "taps created"  docker compose exec -T netbase sh -c 'test "$(ip -br link | grep -c vif)" -ge 16'
}

phase_b_image() {
	echo "=== Phase B: image availability (needs real agent creds) ==="
	echo "images known to ukpd:"
	api "${API}/images" | head -c 800; echo
	echo "(if empty: check agent logs — docker compose logs agent)"
}

phase_c_instance() {
	echo "=== Phase C: instance lifecycle ==="
	local img="${UKP_TEST_IMAGE:-official/httpserver-go121:latest}"
	create=$(api -X POST "${API}/instances" -d "{\"image\":\"${img}\",\"memory_mb\":256,\"autostart\":true}")
	echo "create: ${create}" | head -c 400; echo
	uuid=$(echo "${create}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["uuid"])' 2>/dev/null)
	check "instance created" test -n "${uuid}"
	[ -z "${uuid}" ] && return

	sleep 3
	info=$(api "${API}/instances/${uuid}")
	state=$(echo "${info}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["state"])' 2>/dev/null)
	boot_us=$(echo "${info}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0].get("boot_time_us","?"))' 2>/dev/null)
	check "instance running" test "${state}" = running
	echo "boot_time_us: ${boot_us}  (native lab box reference: ~81904)"
	check "firecracker process exists" docker compose exec -T ukpd pgrep -x firecracker

	# NOTE: lifecycle verbs are PUT (POST returns "No API endpoint")
	api -X PUT "${API}/instances/${uuid}/stop" >/dev/null
	sleep 3
	state=$(api "${API}/instances/${uuid}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["state"])' 2>/dev/null)
	echo "state after stop: ${state}"

	api -X PUT "${API}/instances/${uuid}/start" >/dev/null
	sleep 3
	echo "wake/start round-trip complete"
}

phase_d_blast_radius() {
	echo "=== Phase D: ukpd container restart blast radius ==="
	before=$(docker compose exec -T ukpd pgrep -cx firecracker 2>/dev/null || echo 0)
	echo "firecracker processes before restart: ${before}"
	docker compose restart ukpd
	sleep 10
	after=$(docker compose exec -T ukpd pgrep -cx firecracker 2>/dev/null || echo 0)
	echo "firecracker processes after restart:  ${after}"
	echo "instance states after restart:"
	api "${API}/instances" | head -c 800; echo
	echo "(expected finding: guests die with the container — record for the evaluation)"
}

phase_e_minimal() {
	echo "=== Phase E: Datum-minimal profile (no firewall/openresty/coredns/lego) ==="
	# Datum owns ingress (fabric -> TAP) and policy enforcement, so the
	# target pod is just netsetup + ukpd + agent. This phase measures what
	# actually breaks without the rest.
	docker compose down >/dev/null 2>&1
	docker compose up -d netbase seed netsetup ukpd agent
	for i in $(seq 1 60); do
		state=$(docker compose ps ukpd --format '{{.Health}}' 2>/dev/null)
		[ "$state" = "healthy" ] && break
		sleep 2
	done
	check "ukpd healthy without proxy stack" test "$state" = healthy
	check "api on loopback" api -f "${API}/instances"

	echo "-- nft map coupling: create+start an instance, watch for nft/ipset errors"
	local img="${UKP_TEST_IMAGE:-official/httpserver-go121:latest}"
	create=$(api -X POST "${API}/instances" -d "{\"image\":\"${img}\",\"memory_mb\":256,\"autostart\":true,\"scale_to_zero\":{\"policy\":\"idle\",\"stateful\":true,\"cooldown_time_ms\":3000}}")
	uuid=$(echo "${create}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["uuid"])' 2>/dev/null)
	check "instance created (minimal mode)" test -n "${uuid}"
	sleep 3
	docker compose exec -T ukpd sh -c 'grep -iE "nft|ipset|netlink|EPERM|ENOENT" /var/log/ukp/platform/ukpd.log | tail -5' || true

	if [ -n "${uuid}" ]; then
		gip=$(api "${API}/instances/${uuid}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["network_interfaces"][0]["private_ip"])' 2>/dev/null)
		check "guest reachable directly on TAP" docker compose exec -T netbase curl -s -m 5 -o /dev/null "http://${gip}:8080/"

		echo "-- proxy-less standby wake: waiting for idle cooldown..."
		for i in $(seq 1 30); do
			state=$(api "${API}/instances/${uuid}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["state"])' 2>/dev/null)
			[ "$state" = "standby" ] && break
			sleep 5
		done
		check "instance reached standby" test "$state" = standby
		start_ns=$(date +%s%N)
		docker compose exec -T netbase curl -s -m 10 -o /dev/null "http://${gip}:8080/"
		end_ns=$(date +%s%N)
		state=$(api "${API}/instances/${uuid}" | docker compose exec -T netbase python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["instances"][0]["state"])' 2>/dev/null)
		check "woke via direct TAP packet (no proxy)" test "$state" = running
		echo "wake round-trip: $(( (end_ns - start_ns) / 1000000 )) ms (includes curl overhead)"
	fi
}

case "${1:-all}" in
	a) phase_a_bringup;;
	b) phase_b_image;;
	c) phase_c_instance;;
	d) phase_d_blast_radius;;
	e) phase_e_minimal;;
	all) phase_a_bringup; phase_b_image; phase_c_instance; phase_d_blast_radius; phase_e_minimal;;
esac

echo "=== result: ${PASS} pass, ${FAIL} fail ==="
exit $((FAIL > 0))
