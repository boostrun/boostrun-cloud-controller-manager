# BoostRun Cloud Controller Manager

Provisions [BoostRun](https://boostrun.com) load balancers for Kubernetes Services of
`type: LoadBalancer`. It handles load balancers only, so it works on any Kubernetes distribution
without kubelet changes.

## Requirements

- Kubernetes 1.34 or later (tested on 1.34 to 1.37).
- Nodes are BoostRun rentals in one network cluster, and each node's `InternalIP` is the rental's
  internal IP. Check with:
  ```bash
  kubectl get nodes -o custom-columns=NAME:.metadata.name,IP:'.status.addresses[?(@.type=="InternalIP")].address'
  ```
  If they differ, set the kubelet's `--node-ip`.
- Load balancers are enabled for your organization, and you have an API key with the
  `load-balancer` role, created by an organization admin.
- No other controller serves LoadBalancer Services. Remove MetalLB or kube-vip's cloud provider,
  and start k3s with `--disable servicelb`.

## Install

```bash
kubectl -n kube-system create secret generic boostrun-cloud-controller-manager \
  --from-literal=api-key=<API key>

helm install boostrun-cloud-controller-manager \
  oci://public.ecr.aws/boostrun/charts/boostrun-cloud-controller-manager \
  -n kube-system --set clusterName=<cluster name>
```

Without Helm:

```bash
kubectl apply -f https://github.com/boostrun/boostrun-cloud-controller-manager/releases/latest/download/install.yaml
```

If your organization has more than one network cluster, also set `networkClusterID`. See
[values.yaml](charts/boostrun-cloud-controller-manager/values.yaml) for all options.

## Usage

```yaml
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  type: LoadBalancer
  selector:
    app: web
  ports:
    - {name: http, port: 80, targetPort: 8080}
    - {name: dns, port: 53, targetPort: 5353, protocol: UDP}
```

`EXTERNAL-IP` is `<pending>` until the load balancer is ready, usually within a minute. If it stays
pending, run `kubectl describe service web` to see the reason.

- Each Service port is a separate BoostRun load balancer, billed and counted against your
  organization's limit. All ports of a Service share one IP address.
- In the platform they are named `k8s-<service UID>-<protocol>-<port>`.
- To keep an address across a delete and recreate, set `spec.loadBalancerIP` to it. Services that
  set the same address share it, as long as their ports differ.
- To remove a node from every load balancer, label it
  `node.kubernetes.io/exclude-from-external-load-balancers=true`.

### Annotations

| Annotation | Values | Default |
|---|---|---|
| `loadbalancer.boostrun.com/healthcheck-type` | `tcp`, `http`, `none` | `tcp` (UDP ports are not checked) |
| `loadbalancer.boostrun.com/healthcheck-path` | Path for `http` checks | `/` |
| `loadbalancer.boostrun.com/healthcheck-timeout-ms` | 501 to 60000 | `2000` |
| `loadbalancer.boostrun.com/network-cluster-id` | Network cluster ID | `networkClusterID`, or your only network cluster |
| `loadbalancer.boostrun.com/ip-family` | `ipv4`, `ipv6` | The Service's first IP family |

The network cluster and IP family can't change after creation. Recreate the Service to change them.

### Not supported

These are rejected with an event on the Service:

- `externalTrafficPolicy: Local`. Use `Cluster`.
- `loadBalancerSourceRanges`. The load balancer can't enforce it, so restrict access in the
  cluster instead (for example with a NetworkPolicy). Adding it to a Service that is already
  serving **disables its load balancers**, though `EXTERNAL-IP` still shows the address; removing
  it enables them again.
- SCTP.
- `allocateLoadBalancerNodePorts: false` without explicit node ports. Each port needs a NodePort.

Dual-stack Services get an address for their first IP family only. Services with a
`loadBalancerClass` are ignored.

## Troubleshooting

```bash
kubectl describe service <name>
kubectl -n kube-system logs -l app.kubernetes.io/name=boostrun-cloud-controller-manager
```

| Message | Fix |
|---|---|
| `... one of your active rentals` | A node's `InternalIP` is not its rental's internal IP. See Requirements. |
| `several network clusters` | Set `networkClusterID` or the `network-cluster-id` annotation. |
| `You have reached the limit` | Your organization has no load balancers left. Each Service port uses one. |
| `requested address ... is not available` | The `spec.loadBalancerIP` address is taken or isn't yours. |
| `platform API returned 401` or `403` | The API key is wrong, revoked, or lacks the `load-balancer` role. |

If nothing happens at all, another controller probably owns the Service.

## Upgrade and uninstall

Upgrade with `helm upgrade` or by applying the new `install.yaml`. Existing load balancers are kept.

Delete your LoadBalancer Services before uninstalling. Otherwise their load balancers keep running,
and keep being billed, until you delete them in the platform.

## Security and license

Report vulnerabilities as described in [SECURITY.md](SECURITY.md). Release images are signed with
cosign. Licensed under [Apache 2.0](LICENSE).
