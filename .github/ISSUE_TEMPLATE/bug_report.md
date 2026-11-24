---
name: Bug report
about: Something doesn't work as documented
labels: bug
---

**What happened, and what you expected**

**Steps to reproduce** (the Service manifest helps)

**Versions**
- Controller:
- Kubernetes and distribution:

**Service events and controller logs**
```
kubectl describe service <name>
kubectl -n kube-system logs -l app.kubernetes.io/name=boostrun-cloud-controller-manager
```
Remove API keys and other secrets before posting.
