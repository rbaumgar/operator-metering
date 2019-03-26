#!/bin/bash
set -e

: "${KUBECONFIG:?}"

ROOT_DIR=$(dirname "${BASH_SOURCE}")/..
source "${ROOT_DIR}/hack/common.sh"
source "${ROOT_DIR}/hack/lib/tests.sh"

export METERING_NAMESPACE="${METERING_E2E_NAMESPACE:=${METERING_NAMESPACE}-e2e}"

export DEPLOY_SCRIPT="${DEPLOY_SCRIPT:-deploy-e2e.sh}"
export TEST_SCRIPT="$ROOT_DIR/hack/run-e2e-tests.sh"

export TEST_LOG_FILE="${TEST_LOG_FILE:-e2e-tests.log}"
export DEPLOY_LOG_FILE="${DEPLOY_LOG_FILE:-e2e-deploy.log}"
export TEST_TAP_FILE="${TEST_TAP_FILE:-e2e-tests.tap}"

echo "\$KUBECONFIG=$KUBECONFIG"
echo "\$METERING_NAMESPACE=$METERING_NAMESPACE"
echo "\$METERING_OPERATOR_DEPLOY_REPO=$METERING_OPERATOR_DEPLOY_REPO"
echo "\$REPORTING_OPERATOR_DEPLOY_REPO=$REPORTING_OPERATOR_DEPLOY_REPO"
echo "\$METERING_OPERATOR_DEPLOY_TAG=$METERING_OPERATOR_DEPLOY_TAG"
echo "\$REPORTING_OPERATOR_DEPLOY_TAG=$REPORTING_OPERATOR_DEPLOY_TAG"

export DISABLE_PROMSUM=false
if [ "$DEPLOY_REPORTING_OPERATOR_LOCAL" == "true" ]; then
    export REPORTING_OPERATOR_PROMETHEUS_DATASOURCE_MAX_IMPORT_BACKFILL_DURATION='15m'
    export REPORTING_OPERATOR_PROMSUM_INTERVAL="30s"
    export REPORTING_OPERATOR_PROMSUM_CHUNK_SIZE="5m"
    export REPORTING_OPERATOR_PROMSUM_INTERVAL="5m"
    export REPORTING_OPERATOR_PROMSUM_STEP_SIZE="60s"

    export REPORTING_OPERATOR_API_LISTEN="127.0.0.1:8100"
    export REPORTING_OPERATOR_METRICS_LISTEN="127.0.0.1:8101"
    export REPORTING_OPERATOR_PPROF_LISTEN="127.0.0.1:8102"

    export METERING_PRESTO_PORT_FORWARD_PORT="8103"
    export METERING_HIVE_PORT_FORWARD_PORT="8104"
    export METERING_PROMETHEUS_PORT_FORWARD_PORT="8105"

    export METERING_HTTPS_API="false"
    export METERING_REPORTING_API_URL="http://$REPORTING_OPERATOR_API_LISTEN"
    export METERING_USE_KUBE_PROXY_FOR_REPORTING_API="false"
fi

if [ "$DEPLOY_METERING_OPERATOR_LOCAL" == "true" ]; then
    export METERING_OPERATOR_CONTAINER_NAME="metering-operator-e2e"
fi

"$ROOT_DIR/hack/e2e-test-runner.sh"
