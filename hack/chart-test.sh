#!/usr/bin/env bash
# Render the chart with `helm template` and check the controller.watchTLSSecrets
# switch:
#   false (default) -> no --watch-tls-secrets flag, no Secret access in the ClusterRole
#   true            -> flag on the controller container, Secret get/list/watch granted
# Needs only helm, grep and awk. Run from anywhere: ./hack/chart-test.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)/deploy/helm/nvelox-ingress-controller"
fail=0

render() { # $1 = template path, rest = extra helm args
	local tpl="$1"
	shift
	helm template t "$CHART" --namespace nvelox-ingress --show-only "$tpl" "$@"
}

# Controller container args only: from "- name: controller" up to the next container.
controller_block() {
	awk '/^        - name: controller$/{on=1; print; next} on && /^        - name: /{on=0} on'
}

check() { # $1 = description, $2 = 0 if the check passed
	if [ "$2" -eq 0 ]; then
		echo "ok   - $1"
	else
		echo "FAIL - $1"
		fail=1
	fi
}

has() { grep -q -e "$1" && echo 0 || echo 1; }
lacks() { grep -q -e "$1" && echo 1 || echo 0; }

dep_off="$(render templates/deployment.yaml | controller_block)"
rbac_off="$(render templates/rbac.yaml)"
dep_on="$(render templates/deployment.yaml --set controller.watchTLSSecrets=true | controller_block)"
rbac_on="$(render templates/rbac.yaml --set controller.watchTLSSecrets=true)"

[ -n "$dep_off" ] || { echo "FAIL - controller container not found in rendered Deployment"; exit 1; }

check "default: controller args have no --watch-tls-secrets" "$(printf '%s\n' "$dep_off" | lacks '--watch-tls-secrets')"
check "default: ClusterRole grants no Secret access" "$(printf '%s\n' "$rbac_off" | lacks '"secrets"')"
check "watchTLSSecrets=true: controller args have --watch-tls-secrets=true" "$(printf '%s\n' "$dep_on" | has '- --watch-tls-secrets=true$')"
check "watchTLSSecrets=true: ClusterRole grants Secret get/list/watch" "$(printf '%s\n' "$rbac_on" |
	awk '/resources: \["secrets"\]/{getline v; if (v ~ /verbs: \["get", "list", "watch"\]/) found=1} END{exit !found}' && echo 0 || echo 1)"

exit "$fail"
