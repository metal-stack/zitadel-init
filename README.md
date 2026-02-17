# zitadel-init

This repository contains the initialization scripts and configurations for setting up a default application in Zitadel. It uses the official Zitadel go-sdk to create and configure the application. In addition it also saves the client_id and client_secret to a k8s secret.

Main usage is to bootstrap a default application in the mini-lab.

## Development against mini-lab

```bash
make zitadel-init
./bin/zitadel-init-linux-amd64 \
    --zitadel-endpoint=zitadel.172.17.0.1.nip.io \
    --zitadel-external-domain=zitadel.172.17.0.1.nip.io \
    --zitadel-port=8080 \
    --zitadel-pat=$(kubectl -n metal-control-plane get secrets iam-admin-pat -o jsonpath='{.data.pat}' | base64 -d ) \
    --namespace=metal-control-plane \
    --secret=zitadel-client-credentials \
    --zitadel-skip-verify-tls=True \
    --zitadel-insecure=True \
    --config-path=./examples/config.yaml
```
