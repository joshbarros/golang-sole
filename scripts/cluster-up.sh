#!/usr/bin/env bash
# Creates the kind cluster if it is not there, loads the two images, applies the manifests and waits until the stack answers
# on its NodePorts. The images must exist (make images). Safe to run again: it restarts the Deployments so new images are picked up.
set -euo pipefail

CLUSTER="${CLUSTER:-golang-sole}"
CTX="kind-$CLUSTER"
K="kubectl --context $CTX -n sole"
cd "$(dirname "$0")/.."

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --config deploy/kind/cluster.yaml
kind load docker-image golang-sole-api:dev golang-sole-web:dev --name "$CLUSTER"
./scripts/kind-secrets.sh

existed=false
$K get deployment/api >/dev/null 2>&1 && existed=true
kubectl --context "$CTX" apply -k deploy/k8s/overlays/kind
$K rollout status statefulset/postgres statefulset/kafka statefulset/rabbitmq --timeout=300s
if [ "$existed" = true ]; then $K rollout restart deployment/api deployment/web; fi
$K rollout status deployment/api deployment/web --timeout=300s

# kube-proxy and the kind port mappings can lag a few seconds behind the rollout: wait for a stretch of answers, not one.
ok=0
for _ in $(seq 1 120); do
  if curl -sf http://127.0.0.1:5228/api/health/ready >/dev/null \
     && [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:5228/api/raffles)" = 401 ] \
     && curl -sf http://127.0.0.1:8108/ >/dev/null \
     && [ "$(curl -s -o /dev/null -w '%{http_code}' -u platform:platform http://127.0.0.1:15718/api/overview)" = 200 ]; then
    ok=$((ok + 1))
  else
    ok=0
  fi
  if [ "$ok" -ge 8 ]; then
    echo "ready: web http://127.0.0.1:8108, api http://127.0.0.1:5228, RabbitMQ management http://127.0.0.1:15718"
    exit 0
  fi
  sleep 1
done
echo "the NodePorts never answered steadily" >&2
exit 1
