#!/usr/bin/env bash
# Installs the Helm chart on the current kind cluster against test/fakeplatform and checks the
# controller end to end: RBAC, probes and leader election work, a LoadBalancer Service gets an
# address with the eligible nodes as backends, a node that is not a rental is skipped with a
# warning event, an unsupported Service is rejected with an event, and deleting the Service
# removes its load balancers.
#
# Usage: test/kind/install-test.sh IMAGE   (an image already loaded into the kind cluster)
set -euo pipefail

IMAGE=${1:?usage: install-test.sh IMAGE}
NS=kube-system
RELEASE=boostrun-cloud-controller-manager
KEY=kind-test-key
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
FAKE_PORT=18080

fail() {
	echo "FAIL: $*" >&2
	echo "--- controller logs" >&2
	kubectl -n "$NS" logs -l app.kubernetes.io/instance="$RELEASE" --tail=80 --prefix >&2 || true
	echo "--- fake platform log" >&2
	tail -40 "$WORK/fake.log" >&2 || true
	exit 1
}

# waitfor SECONDS DESCRIPTION COMMAND...: retries COMMAND every 2 s until it succeeds.
waitfor() {
	local deadline=$((SECONDS + $1)) what=$2
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || fail "timed out waiting for $what"
		sleep 2
	done
	echo "ok: $what"
}

state() { curl -fsS "http://127.0.0.1:$FAKE_PORT/debug/state"; }

go build -o "$WORK/fakeplatform" "$ROOT/test/fakeplatform"
"$WORK/fakeplatform" -listen "0.0.0.0:$FAKE_PORT" -api-key "$KEY" >"$WORK/fake.log" 2>&1 &
trap 'kill %1 2>/dev/null || true' EXIT
waitfor 20 "fake platform" state

# Pods reach the host through the kind network's gateway.
GATEWAY=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' |
	tr ' ' '\n' | grep -m1 -E '^[0-9]+\.')
[ -n "$GATEWAY" ] || fail "no IPv4 gateway on the kind network"

kubectl -n "$NS" create secret generic "$RELEASE" --from-literal=api-key="$KEY"
helm install "$RELEASE" "$ROOT/charts/boostrun-cloud-controller-manager" -n "$NS" \
	--set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
	--set apiURL="http://$GATEWAY:$FAKE_PORT/api/v1" --set clusterName=kind-ci --set logLevel=4 \
	--wait --timeout 3m || fail "helm install"
echo "ok: chart installed, both replicas ready"

waitfor 60 "a leader lease" kubectl -n "$NS" get lease "$RELEASE"

# kind labels its control plane node.kubernetes.io/exclude-from-external-load-balancers. Of the
# remaining nodes, the platform reports only the first as a rental.
mapfile -t NODES < <(kubectl get nodes -l '!node.kubernetes.io/exclude-from-external-load-balancers' \
	-o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | sort)
[ "${#NODES[@]}" -ge 2 ] || fail "need two nodes eligible for load balancing, got ${#NODES[@]}"
RENTAL=${NODES[0]}
SKIPPED=${NODES[1]}
curl -fsS -X PUT "http://127.0.0.1:$FAKE_PORT/debug/rentals" -d "[\"$RENTAL\"]" >/dev/null

kubectl create deployment echo --image=registry.k8s.io/pause:3.10 >/dev/null
kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata: {name: web, namespace: default}
spec:
  type: LoadBalancer
  selector: {app: echo}
  ports:
    - {name: http, port: 80, targetPort: 8080}
    - {name: dns, port: 53, targetPort: 5353, protocol: UDP}
YAML
external_ip() { kubectl get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}' | grep -q .; }
waitfor 120 "EXTERNAL-IP on Service web" external_ip

UID_=$(kubectl get svc web -o jsonpath='{.metadata.uid}')
state | jq -e --arg p "k8s-$UID_-" --arg rental "$RENTAL" '
	[.[] | select(.name | startswith($p))] as $lbs
	| ($lbs | length) == 2
	and ($lbs | map(.status) | unique) == ["active"]
	and ($lbs | map(.frontend_ip) | unique | length) == 1
	and ($lbs | all(.description == "Kubernetes Service default/web in cluster kind-ci"))
	and ($lbs | all(.network_cluster_id == "nc-kind"))
	and ($lbs | all([.backends[].internal_ip] == [$rental]))' >/dev/null ||
	{ state | jq . >&2; fail "platform state for Service web (want 2 active LBs on one address, only $RENTAL as backend)"; }
echo "ok: 2 load balancers on one address, only the rental node as backend, cluster named in the description"

skipped_event() { kubectl get events --field-selector involvedObject.name=web -o json |
	jq -e --arg ip "$SKIPPED" '[.items[] | select(.reason == "LoadBalancerNodesSkipped") | .message] | any(contains($ip))'; }
waitfor 60 "LoadBalancerNodesSkipped event naming $SKIPPED" skipped_event

# Source ranges can't be enforced, so adding them to a serving Service disables its load balancers
# until they are removed.
web_enabled() { state | jq -e --arg p "k8s-$UID_-" --argjson want "$1" \
	'[.[] | select(.name | startswith($p)) | .enabled] | length == 2 and all(. == $want)'; }
kubectl patch svc web -p '{"spec":{"loadBalancerSourceRanges":["192.0.2.0/24"]}}' >/dev/null
waitfor 60 "web's load balancers disabled by source ranges" web_enabled false
kubectl patch svc web --type=json -p '[{"op":"remove","path":"/spec/loadBalancerSourceRanges"}]' >/dev/null
waitfor 60 "web's load balancers enabled again" web_enabled true

kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata: {name: restricted, namespace: default}
spec:
  type: LoadBalancer
  loadBalancerSourceRanges: [192.0.2.0/24]
  selector: {app: echo}
  ports: [{port: 80, targetPort: 8080}]
YAML
rejected() { kubectl get events --field-selector involvedObject.name=restricted -o json |
	jq -e '[.items[].message] | any(test("loadBalancerSourceRanges is not supported"))'; }
waitfor 60 "rejection event on Service restricted" rejected
RESTRICTED_UID=$(kubectl get svc restricted -o jsonpath='{.metadata.uid}')
state | jq -e --arg p "k8s-$RESTRICTED_UID-" '[.[] | select(.name | startswith($p))] | length == 0' >/dev/null ||
	fail "Service restricted got platform load balancers"
echo "ok: Service restricted has no platform load balancers"

kubectl delete svc web restricted --wait=true
gone() { state | jq -e 'length == 0'; }
waitfor 60 "every platform load balancer deleted" gone

restarts=$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance="$RELEASE" \
	-o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{s += $1} END {print s + 0}')
[ "$restarts" = 0 ] || fail "controller containers restarted $restarts time(s)"
logs=$(kubectl -n "$NS" logs -l app.kubernetes.io/instance="$RELEASE" --tail=-1) || fail "reading controller logs"
if grep -E 'panic|forbidden' <<<"$logs"; then
	fail "controller logged a panic or a forbidden API call"
fi
echo "PASS"
