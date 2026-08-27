#!/bin/bash -e
# Seeds the ukp data volume for the local prototype: self-signed certs with
# the exact names the openresty launcher checks, and a users.json with a
# known auth token so the API can be exercised.
. /etc/ukp.conf

CERTS="${UKP_DATA}/certificates"
mkdir -p "${CERTS}"

gen_cert() {
	local name="$1" cn="$2"
	[ -e "${CERTS}/${name}.key" ] && return 0
	echo "seeding cert ${name}"
	openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
		-keyout "${CERTS}/${name}.key" -out "${CERTS}/${name}.crt" \
		-subj "/CN=${cn}" 2>/dev/null
}

gen_cert "_.${DNS_HOSTNAME}.${DNS_ZONE_APP}"   "*.${DNS_HOSTNAME}.${DNS_ZONE_APP}"
gen_cert "api.${DNS_HOSTNAME}.${DNS_ZONE_API}"   "api.${DNS_HOSTNAME}.${DNS_ZONE_API}"
gen_cert "index.${DNS_HOSTNAME}.${DNS_ZONE_API}" "index.${DNS_HOSTNAME}.${DNS_ZONE_API}"

if [ ! -e "${UKP_USERDB}" ]; then
	echo "seeding users.json"
	mkdir -p "$(dirname "${UKP_USERDB}")"
	cat > "${UKP_USERDB}" <<'JSON'
[
	{
		"uuid": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		"name": "local",
		"auth_token": "bG9jYWw6bG9jYWx0ZXN0",
		"network_id": 0,
		"autoscale": {
			"min_size": 0,
			"max_size": 4
		},
		"vmdb": {
			"max_vcpus": 4,
			"max_memory_mb": 8192,
			"max_instances": 32
		},
		"net": {
			"max_service_groups": 64,
			"max_services": 64
		},
		"vmm": {
			"max_vcpus": 8,
			"max_memory_mb": 8192
		},
		"stor": {
			"max_volumes": 64,
			"max_volume_mb": 4096,
			"max_total_volume_mb": 16384
		}
	}
]
JSON
fi

echo "seed complete"
