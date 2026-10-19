# Kubernetes

`deploy/k8s` runs the compose `demo` profile (docs/demo.md) on Kubernetes:
Postgres, the control plane, bmcsim, chaos, four fake-GPU agents, Prometheus,
Alertmanager and Grafana, all in the `gpu-fleet` namespace. It targets a local
[kind](https://kind.sigs.k8s.io) cluster, and CI runs it end to end on every
push (`k8s-smoke` in `.github/workflows/ci.yml`).

## Run it on kind

```bash
docker compose -f deploy/compose.yaml --profile demo build
kind create cluster --name gpu-fleet
for image in $(docker compose -f deploy/compose.yaml --profile demo config --images | grep '^gpu-fleet-'); do kind load docker-image "$image" --name gpu-fleet; done
kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/k8s | kubectl apply -f -
kubectl -n gpu-fleet wait deployment --all --for=condition=Available --timeout=5m
```

Then forward the ports you want:

```bash
kubectl -n gpu-fleet port-forward svc/controlplane 8080:8080
kubectl -n gpu-fleet port-forward svc/grafana 3000:3000
```

Remove everything with `kind delete cluster --name gpu-fleet`.

## Layout

| File | What |
|---|---|
| `kustomization.yaml` | namespace, dev-only Secrets, ConfigMaps built from the shared config files |
| `postgres.yaml` | Postgres 17 on an `emptyDir` (data does not survive the pod) |
| `controlplane.yaml`, `bmcsim.yaml`, `chaos.yaml` | the three Go binaries from `controlplane/Dockerfile` |
| `agents.yaml` | `agent-node-001`..`004`, one Service each, plus the headless Service `agent` |
| `prometheus.yaml`, `alertmanager.yaml`, `grafana.yaml` | the observability stack |
| `prometheus.yml`, `alertmanager.yml` | Kubernetes variants of the compose configs |

## Design

- **Same images, same names.** The manifests use the compose image tags
  (`gpu-fleet-*:dev` and the pinned upstream images); CI fails if the two
  image lists drift. Service names match compose service names, so chaos and
  Grafana need no configuration and Prometheus and Alertmanager only swap
  `host.docker.internal:8080` for `controlplane:8080`.
- **Shared config, one copy.** Alert rules, the dashboard and Grafana
  provisioning are read from `deploy/prometheus` and `deploy/grafana`, which
  sit outside `deploy/k8s`. That is why the build needs
  `--load-restrictor LoadRestrictionsNone`, and why `kubectl apply -k` (which
  cannot pass that flag) is not used.
- **Agents as Services.** Compose gives every agent the network alias `agent`
  for Prometheus DNS discovery. Here a headless Service named `agent` selects
  all agent pods, so the same `dns_sd_configs` entry finds them.
- **Startup order** is left to restarts, as in compose: the control plane
  exits until Postgres accepts connections and Kubernetes restarts it.
- **Dev only.** Secrets are the compose dev values, Postgres has no volume,
  and nothing is exposed outside the cluster. A real cluster would add an
  overlay with a registry for the images, real Secrets and persistent storage.
